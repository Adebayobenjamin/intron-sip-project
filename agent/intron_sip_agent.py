"""
intron_sip_agent — the AI agent for calls arriving on the Intron SIP platform.

RUNS ON INTRON'S GPU BOX (Calls05), alongside — not inside — their own stack.

WHAT IT IS
    A LiveKit agent worker that registers with the SIP platform's LiveKit and
    answers the calls it is dispatched: speech-to-text, an LLM, text-to-speech,
    and two tools — hand the caller to a human, and end the call.

WHAT IT REUSES, AND WHAT IT DOES NOT
    It reuses Intron's MODELS through their own LiveKit wrappers, imported
    read-only from their checkout:

        STT  their fine-tuned NVIDIA TDT model      service_convobot_libs_models_stt_tdt
        TTS  their OmniVoice checkpoint             service_convobot_libs_models_tts_omnivoice
        LLM  Gemini, with their Google API key      service_convobot_libs_models_llm_gemini
        VAD  Silero

    It does NOT reuse their ORCHESTRATION. Their worker loads a per-call
    configurations.json written by their outbound dialer, restarts once per
    call, and reads its prompt from a call row in their database — none of
    which exists for a call that arrives from the network, which is why their
    worker cannot take inbound calls at all. This worker's settings come from
    its own environment and a prompt file, and it stays up across calls.

    Nothing here writes to Intron's checkout, database or configuration.

ONE CALL AT A TIME, ON PURPOSE
    The models are loaded once, in this process, and every call runs as a
    thread inside it (the same arrangement their worker uses: CUDA models
    cannot be shared with forked processes). Their wrappers were written for
    one call per process and are not known to be safe for two at once, and the
    box has one T4. So the worker reports itself FULL while AGENT_MAX_CALLS
    calls are running and LiveKit dispatches elsewhere — or, with no other
    worker, the caller hears silence until the call times out. Raise it only
    after testing two concurrent calls.

RECORDING
    None here. The SIP platform records on FreeSWITCH, on the caller's leg,
    which also covers the part of a call after a human takes over. In
    particular there is no room-composite egress call: with no egress service
    on the platform's LiveKit it stalls ~23s before failing, and the caller
    hangs up first.
"""

from __future__ import annotations

import asyncio
import logging
import os
import sys
import threading

# Their checkout, for the model wrappers. Inserted before any import of theirs.
APP_ROOT = os.environ.get("AGENT_APP_ROOT", "/home/ubuntu/service_voice_bots")
if APP_ROOT not in sys.path:
    sys.path.insert(0, APP_ROOT)

from livekit import api, rtc  # noqa: E402
from livekit.agents import (  # noqa: E402
    Agent,
    AgentSession,
    AutoSubscribe,
    JobContext,
    JobExecutorType,
    RunContext,
    WorkerOptions,
    cli,
    function_tool,
)
from livekit.plugins import silero  # noqa: E402

log = logging.getLogger("intron-sip-agent")


# =============================================================================
# Configuration
#
# Every value has a default that matches what is on Calls05 today, so the only
# REQUIRED environment is LiveKit's (LIVEKIT_URL / _API_KEY / _API_SECRET, read
# by the SDK) and GOOGLE_API_KEY (read by the Gemini plugin).
# =============================================================================


def env(name: str, default: str) -> str:
    v = os.environ.get(name, "").strip()
    return v if v else default


# Our LiveKit's dispatch rule summons the agent by this name. Intron's own
# worker uses the same name, but on THEIR LiveKit server, so the two never meet.
AGENT_NAME = env("AGENT_NAME", "intron_convobot_agent_2")

# The livekit-agents health server. 8081 is their worker's; 8082 was the old
# bridge's. Two workers on one port: the second dies at startup.
AGENT_HTTP_PORT = int(env("AGENT_HTTP_PORT", "8083"))

AGENT_MAX_CALLS = int(env("AGENT_MAX_CALLS", "1"))

# Where a transfer goes. NOT a phone number: "handoff" is a reserved word that
# tells the platform a human is wanted, and the platform picks the destination
# from the tenant's handoff targets. The host is the edge's PRIVATE address —
# the REFER travels inside the platform's VPC.
AGENT_TRANSFER_TO = env("AGENT_TRANSFER_TO", "sip:handoff@10.70.0.10")

AGENT_INSTRUCTIONS_FILE = env(
    "AGENT_INSTRUCTIONS_FILE",
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "instructions.txt"),
)
AGENT_GREETING = env(
    "AGENT_GREETING", "Hello, thank you for calling. How can I help you today?"
)

# The longest a last sentence is given to finish before a hang-up or transfer
# goes ahead anyway. The agent waits for its own speech to end; this is the cap.
AGENT_HANGUP_GRACE = float(env("AGENT_HANGUP_GRACE", "8"))

# Silence, in seconds, before the caller is asked whether they are still there.
AGENT_USER_AWAY_TIMEOUT = float(env("AGENT_USER_AWAY_TIMEOUT", "15"))

# The models. Defaults are the files present on Calls05; their manifest names
# different English paths that are not on disk, which is why these are given
# explicitly rather than read from it.
MODELS = os.path.join(APP_ROOT, "service_convo_bot", "models")
STT_MODEL_PATH = env(
    "AGENT_STT_MODEL_PATH",
    os.path.join(MODELS, "models_2025/nvidia/best_tdt_v2_model_sept_30_24.nemo"),
)
TTS_MODEL_PATH = env(
    "AGENT_TTS_MODEL_PATH",
    "/home/ubuntu/models/TTS/omnivoice_intron_2026_08_02/checkpoint-360000",
)
TTS_REF_AUDIO = env(
    "AGENT_TTS_REF_AUDIO", "/home/ubuntu/omnivoice-voice-refs/yoruba_female1_ref.wav"
)
# The words spoken in the reference clip. Must match the clip, or the cloned
# voice degrades. This is the text their manifest pairs with the default clip.
TTS_REF_TEXT = env(
    "AGENT_TTS_REF_TEXT",
    "Accents are the unique ways of pronouncing words of a particular language",
)
TTS_LANGUAGE = env("AGENT_LANGUAGE", "en")
LLM_MODEL = env("AGENT_LLM_MODEL", "gemini-2.5-flash")
LLM_TEMPERATURE = float(env("AGENT_LLM_TEMPERATURE", "1.0"))


# =============================================================================
# Models — loaded once, before the worker starts, shared by every call
# =============================================================================


class Models:
    stt = None
    tts = None
    vad = None

    # NOT an instance: the factory. See new_llm().
    llm_factory = None


def new_llm():
    """A Gemini client for one call.

    PER CALL, NOT SHARED, unlike the speech models. The client opens an HTTP
    session bound to the event loop of the first call that uses it, and every
    call runs in its own thread with its own loop. A shared client therefore
    works for exactly one call; every later call fails with
    "Timeout context manager should be used inside a task", and the agent
    hears the caller and never answers. Seen on the third call after a restart.

    The speech models do not have this problem: they run their inference in a
    thread pool, not on the loop. And a client is cheap — no weights, no GPU.
    """
    return Models.llm_factory(param_model=LLM_MODEL, param_temperature=LLM_TEMPERATURE)


def load_models() -> None:
    """Load and warm every model in this process.

    Before cli.run_app, not in a prewarm callback: with the THREAD executor,
    calls run inside this process, so models loaded here are the ones they
    use. Warming here means the first caller does not wait ~80s for weights
    to load — the delay their worker's logs show when it starts cold.
    """
    from service_convo_bot import (
        service_convobot_libs_models_llm_gemini as gemini,
        service_convobot_libs_models_stt_tdt as tdt,
        service_convobot_libs_models_tts_omnivoice as omnivoice,
    )

    for label, path in (("STT model", STT_MODEL_PATH), ("TTS model", TTS_MODEL_PATH),
                        ("TTS reference audio", TTS_REF_AUDIO)):
        if not os.path.exists(path):
            raise SystemExit(f"{label} not found: {path}")

    if not os.environ.get("GOOGLE_API_KEY"):
        raise SystemExit("GOOGLE_API_KEY is not set; the Gemini LLM cannot start")

    log.info("loading models")

    # The same arguments their worker passes, taken from the common_configs in
    # their models manifest.
    Models.vad = silero.VAD.load()
    Models.stt = tdt.create_tdt_stt(
        model_path=STT_MODEL_PATH, sample_rate=16000, batch_size=1, vad_threshold=0.5
    )
    Models.tts = omnivoice.create_omnivoice_tts(
        model_path=TTS_MODEL_PATH,
        ref_audio=TTS_REF_AUDIO,
        ref_text=TTS_REF_TEXT,
        language=TTS_LANGUAGE,
        num_steps=16,
        sample_rate=24000,
        device="auto",
        instruct=None,
        design_seed=False,
        model_dtype="float16",
    )
    # Built once here only to fail fast on a bad model name or key; the
    # client each call uses is made by new_llm(), in that call's loop.
    Models.llm_factory = gemini.create_llm_gemini
    new_llm()

    for name, model in (("stt", Models.stt), ("tts", Models.tts)):
        if hasattr(model, "warmup"):
            log.info("warming %s", name)
            asyncio.run(model.warmup())

    log.info("models ready")


def load_instructions() -> str:
    try:
        with open(AGENT_INSTRUCTIONS_FILE, encoding="utf-8") as f:
            text = f.read().strip()
    except OSError as ex:
        raise SystemExit(f"cannot read instructions file {AGENT_INSTRUCTIONS_FILE}: {ex}")
    if not text:
        raise SystemExit(f"instructions file {AGENT_INSTRUCTIONS_FILE} is empty")

    # The tools only work if the model knows when to use them, so how to use
    # them is appended here rather than left to whoever edits the prompt file.
    return text + (
        "\n\nTOOLS. When the caller asks for a person, a human, an agent, or "
        "you cannot help them, call transfer_to_human. When the conversation is "
        "finished — the caller says goodbye, or everything they asked for is "
        "done — call end_call. Do not just say goodbye without calling end_call."
    )


# =============================================================================
# SIP audio
# =============================================================================


def patch_room_io_accept_sip_audio() -> None:
    """Let the agent hear SIP callers.

    livekit-sip publishes the caller's audio track with no explicit source, so
    it arrives as SOURCE_UNKNOWN, and RoomIO accepts only SOURCE_MICROPHONE —
    every word the caller says is silently dropped. Intron's worker carries
    the same patch for the same reason. Remove when the SDK accepts SIP audio.
    """
    try:
        from livekit.agents.voice.room_io import _input as room_io_input

        cls = room_io_input._ParticipantAudioInputStream
        if getattr(cls, "_intron_sip_patch", False):
            return
        original = cls._on_track_available

        def on_track_available(self, track, publication, participant):
            if publication.source == rtc.TrackSource.SOURCE_UNKNOWN:
                self._accepted_sources.add(rtc.TrackSource.SOURCE_UNKNOWN)
            return original(self, track, publication, participant)

        cls._on_track_available = on_track_available
        cls._intron_sip_patch = True
    except Exception:
        # Loud, because the symptom is an agent that talks and never hears.
        log.exception("could not patch RoomIO for SIP audio; callers will not be heard")


# =============================================================================
# Load reporting — one call at a time
# =============================================================================

_active_lock = threading.Lock()
_active_calls = 0


def _call_started() -> None:
    global _active_calls
    with _active_lock:
        _active_calls += 1


def _call_ended() -> None:
    global _active_calls
    with _active_lock:
        _active_calls = max(0, _active_calls - 1)


def current_load(*_args) -> float:
    with _active_lock:
        return min(1.0, _active_calls / max(1, AGENT_MAX_CALLS))


# =============================================================================
# The call
# =============================================================================


async def delete_room(room_name: str) -> None:
    """End the call. livekit-sip sends BYE on the platform leg, and FreeSWITCH
    hangs up the caller because the bridge ended."""
    async with api.LiveKitAPI() as lk:
        await lk.room.delete_room(api.DeleteRoomRequest(room=room_name))


async def wait_until_quiet(session: AgentSession, cap: float) -> None:
    """Wait for the agent to finish what it is saying, up to `cap` seconds.

    Used before a transfer or a hang-up so the caller hears the whole of the
    last sentence. The short head start lets the reply to a tool call begin:
    the tool returns first, and only then does the LLM say the sentence it was
    told to say.
    """
    loop = asyncio.get_running_loop()
    deadline = loop.time() + cap
    await asyncio.sleep(1.5)
    while loop.time() < deadline and session.agent_state in ("thinking", "speaking"):
        await asyncio.sleep(0.2)


async def entrypoint(ctx: JobContext) -> None:
    _call_started()

    async def _on_shutdown() -> None:
        _call_ended()

    ctx.add_shutdown_callback(_on_shutdown)

    await ctx.connect(auto_subscribe=AutoSubscribe.AUDIO_ONLY)

    # The caller. Waiting for the SIP participant specifically, so a browser
    # tester or another agent joining the room is not mistaken for them.
    caller = await ctx.wait_for_participant(kind=rtc.ParticipantKind.PARTICIPANT_KIND_SIP)
    caller_number = caller.attributes.get("sip.phoneNumber", "unknown")
    room = ctx.room.name
    log.info("call started room=%s caller=%s", room, caller_number)

    session = AgentSession(user_away_timeout=AGENT_USER_AWAY_TIMEOUT)

    llm = new_llm()

    async def _close_llm() -> None:
        try:
            await llm.aclose()
        except Exception:
            pass

    ctx.add_shutdown_callback(_close_llm)

    # Set once the call is on its way out — a hang-up or a transfer — so the
    # silence reprompt and the tools stop acting on a call that is leaving.
    ending = False

    async def hang_up(reason: str) -> None:
        nonlocal ending
        if ending:
            return
        ending = True
        log.info("ending call room=%s reason=%s", room, reason)
        await wait_until_quiet(session, AGENT_HANGUP_GRACE)
        try:
            await delete_room(room)
        except Exception:
            log.exception("could not delete room %s", room)

    async def transfer() -> None:
        nonlocal ending
        ending = True
        await wait_until_quiet(session, AGENT_HANGUP_GRACE)

        log.info("transferring room=%s to=%s", room, AGENT_TRANSFER_TO)
        try:
            # A REFER on the platform leg. FreeSWITCH moves the caller to the
            # tenant's human and drops this leg, so on success the caller
            # leaves the room and the session closes on its own.
            async with api.LiveKitAPI() as lk:
                await lk.sip.transfer_sip_participant(
                    api.TransferSIPParticipantRequest(
                        room_name=room,
                        participant_identity=caller.identity,
                        transfer_to=AGENT_TRANSFER_TO,
                        play_dialtone=False,
                    )
                )
            log.info("transfer accepted room=%s", room)
        except Exception as ex:
            # The caller is still here: say so, and carry on with the call.
            log.error("transfer failed room=%s error=%s", room, ex)
            ending = False
            session.say(
                "I'm sorry, nobody is available to take your call right now. "
                "Is there anything else I can help you with?",
                allow_interruptions=True,
            )

    # TOOLS RETURN AT ONCE AND ACT IN THE BACKGROUND. Speaking from inside a
    # tool and waiting for it deadlocks: the new speech is queued behind the
    # turn that called the tool, and that turn cannot finish until the tool
    # returns. So each tool tells the model what to say and schedules the
    # action, which waits for the agent to fall quiet before it runs.

    @function_tool(
        description=(
            "Transfer the caller to a human. Use when the caller asks for a "
            "person, an agent or a human, or when you cannot help them."
        )
    )
    async def transfer_to_human(context: RunContext) -> str:
        if ending:
            return "The call is already ending. Say nothing."
        asyncio.ensure_future(transfer())
        return (
            "Tell the caller, in one short sentence, that you are connecting them "
            "to a colleague now. Say nothing else."
        )

    @function_tool(
        description=(
            "End the call. Use when the conversation is finished: the caller "
            "says goodbye, or everything they asked for is done."
        )
    )
    async def end_call(context: RunContext) -> str:
        if ending:
            return "The call is already ending. Say nothing."
        asyncio.ensure_future(hang_up("end_call_tool"))
        return "Say a brief goodbye. The call will then end."

    agent = Agent(
        instructions=INSTRUCTIONS,
        stt=Models.stt,
        tts=Models.tts,
        llm=llm,
        vad=Models.vad,
        tools=[transfer_to_human, end_call],
        # Their worker's turn handling, unchanged: VAD-based turns, dynamic
        # endpointing between 0.5s and 3s, interruptions on.
        turn_handling={
            "turn_detection": "vad",
            "endpointing": {"mode": "dynamic", "min_delay": 0.5, "max_delay": 3.0},
            "interruption": {"enabled": True},
            "preemptive_generation": {"enabled": True, "preemptive_tts": False},
        },
    )

    reprompted = False

    @session.on("user_state_changed")
    def _on_user_state(ev) -> None:
        nonlocal reprompted
        # An event object in livekit-agents 1.x; tolerate a bare state too.
        state = getattr(ev, "new_state", ev)
        if ending or state != "away":
            return
        if not reprompted:
            reprompted = True
            session.say("Hello? Are you still there?", allow_interruptions=True)
        else:
            asyncio.ensure_future(hang_up("caller_silent"))

    patch_room_io_accept_sip_audio()
    await session.start(agent=agent, room=ctx.room)

    # Not interruptible, so a caller's "hello?" over the top does not cut the
    # greeting off half-way and leave them wondering who answered.
    await session.say(AGENT_GREETING, allow_interruptions=False)


INSTRUCTIONS = ""


def main() -> None:
    global INSTRUCTIONS
    logging.basicConfig(
        level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s"
    )

    INSTRUCTIONS = load_instructions()
    load_models()

    cli.run_app(
        WorkerOptions(
            entrypoint_fnc=entrypoint,
            agent_name=AGENT_NAME,
            # Calls run as threads in this process, where the models live.
            job_executor_type=JobExecutorType.THREAD,
            num_idle_processes=1,
            initialize_process_timeout=240.0,
            load_fnc=current_load,
            # Full means unavailable: at AGENT_MAX_CALLS the load is 1.0.
            load_threshold=0.99,
            port=AGENT_HTTP_PORT,
        )
    )


if __name__ == "__main__":
    main()
