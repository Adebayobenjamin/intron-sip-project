module "livekit" {
  source = "../_main"

  environment         = "beta"
  aws_region          = var.aws_region
  state_bucket        = var.state_bucket
  aws_profile         = var.aws_profile
  state_bucket_region = var.aws_region

  ########################################################
  # The public name.
  #
  # quiverlearn.com is managed outside this account, so this stack creates and
  # owns intron.quiverlearn.com — one level below it — and puts the record
  # there. After the first apply, someone with access to the parent domain
  # adds the four NS records from the `livekit_zone_nameservers` output.
  #
  # THAT DELEGATION IS THE GATE ON EVERYTHING ELSE in this stack. Until it is
  # done the name does not resolve, Caddy cannot complete the ACME challenge,
  # there is no certificate, and Intron's agent fails to connect with a TLS
  # error that says nothing about DNS.
  ########################################################
  public_dns_zone   = "intron.quiverlearn.com"
  livekit_subdomain = "livekit-beta"

  ########################################################
  # Beta topology: one of everything, matching the rest of the platform.
  #
  # t3.small because LiveKit is an SFU — it forwards RTP and neither transcodes
  # nor runs a model, so this is a network-bound workload with a small CPU cost
  # per stream. The models stay on Intron's T4 in eu-west-2 and reach this
  # server as an ordinary WebRTC client; nothing here needs a GPU.
  #
  # What to watch before real traffic is NETWORK rather than CPU. t3 baseline
  # bandwidth is modest and burst credits run out; the symptom is audio that
  # degrades gradually under sustained concurrency rather than a clean failure.
  ########################################################
  livekit_count         = 1
  livekit_instance_type = "t3.small"

  ########################################################
  # Open to the internet, and unlike carrier_sip_cidrs in sip-core that is the
  # correct default rather than an oversight.
  #
  # The clients are a browser on a laptop and an agent process behind a NAT
  # gateway in an account we do not control. There is no stable list to write.
  # What protects this box is that every room join and every API call carries a
  # JWT signed with the API secret — no token, no access. See the long note in
  # ../_main/security-groups.tf before narrowing this.
  ########################################################
  livekit_client_cidrs = ["0.0.0.0/0"]

  ########################################################
  # MUST MATCH rtc.port_range_start/end in the rendered livekit.yaml — see
  # ansible/roles/livekit/templates/livekit.yaml.j2. Terraform opens what the
  # daemon binds to, from a different file, and a mismatch is a call that
  # connects, shows both participants and carries no audio.
  ########################################################
  rtc_udp_port_min = 50000
  rtc_udp_port_max = 60000
}
