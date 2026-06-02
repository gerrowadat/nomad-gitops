# nomad-botherer — example Nomad job definition
#
# Run nomad-botherer as a Nomad service job on the same cluster it watches.
# This file is intended as a starting point: copy it into your job repo,
# edit the env {} block for your environment, and submit with:
#
#   nomad job run nomad-botherer.hcl
#
# The only required setting is GIT_REPO_URL. Everything else has a sensible
# default. Set API_KEY to enable the authenticated JSON API (/api/v1/).

job "nomad-botherer" {
  # Run in the same namespace as the jobs you want to watch. nomad-botherer
  # does not need elevated privileges: read access (list-jobs, read-job) is
  # all that is required.
  namespace   = "default"
  datacenters = ["dc1"]
  type        = "service"

  # Opt this job in to its own monitoring so nomad-botherer watches itself
  # for drift. The meta key name is controlled by MANAGED_META_PREFIX below;
  # with the default prefix of "gitops" the key is "gitops_managed".
  meta {
    gitops_managed = "true"
  }

  group "botherer" {
    # nomad-botherer stores all git state in memory and writes nothing to
    # disk. A single allocation is correct; running more than one produces
    # duplicate drift reports.
    count = 1

    # Restart policy: allow a few quick retries before backing off, so a
    # transient startup error (e.g. git clone timing out) recovers quickly.
    restart {
      attempts = 5
      interval = "5m"
      delay    = "15s"
      mode     = "delay"
    }

    network {
      # HTTP port: /healthz, /metrics, /diffs, /webhook, /api/v1/, and the status page.
      port "http" { to = 8080 }
    }

    task "nomad-botherer" {
      driver = "docker"

      config {
        image = "ghcr.io/gerrowadat/nomad-botherer:latest"
        ports = ["http"]
      }

      env {
        # ── Git source ──────────────────────────────────────────────────────
        #
        # URL of the repository containing your Nomad HCL job definitions.
        # HTTPS with a token (GIT_TOKEN) or SSH with a key file (GIT_SSH_KEY)
        # are both supported. Public repos need no auth.
        GIT_REPO_URL = "https://github.com/myorg/nomad-jobs.git"

        # Branch to watch. Defaults to "main" if not set.
        GIT_BRANCH = "main"

        # Subdirectory within the repo that contains HCL job files.
        # Leave empty (or omit) to use the repo root.
        # HCL_DIR = "jobs"

        # How often to poll the remote for new commits. The default is 5m.
        # Set this shorter only if you are not using webhooks — polling more
        # frequently than your git host rate-limits is counterproductive.
        POLL_INTERVAL = "5m"

        # ── Git authentication ──────────────────────────────────────────────
        #
        # For HTTPS repos: a GitHub PAT, GitLab deploy token, or similar.
        # Pass via a Nomad Variable (recommended) rather than hardcoding here:
        #
        #   nomad var put nomad/jobs/nomad-botherer GIT_TOKEN=ghp_...
        #
        # Then reference it in a template block (see the template section below
        # for an example of reading Nomad Variables into env vars).
        #
        # GIT_TOKEN = "ghp_..."

        # For SSH repos: path to a mounted private key file.
        # Mount the key from a Nomad Variable or Vault secret and point here.
        # GIT_SSH_KEY          = "/secrets/id_ed25519"
        # GIT_SSH_KEY_PASSWORD = ""

        # ── Nomad connection ────────────────────────────────────────────────
        #
        # Address of the Nomad HTTP API. Inside a Nomad cluster the local agent
        # is reachable on the loopback address.
        NOMAD_ADDR = "http://127.0.0.1:4646"

        # ACL token. Required when ACLs are enabled on the cluster.
        # Minimum required capabilities for the token's policy:
        #
        #   namespace "default" {
        #     capabilities = ["list-jobs", "read-job"]
        #   }
        #
        # Use a Nomad Variable or Vault secret rather than hardcoding here.
        # NOMAD_TOKEN = ""

        # Namespace to watch. Defaults to "default".
        # NOMAD_NAMESPACE = "default"

        # ── HTTP server ─────────────────────────────────────────────────────

        LISTEN_ADDR = ":8080"

        # GitHub/GitLab webhook HMAC-SHA256 secret. When set, incoming webhook
        # deliveries are rejected unless their X-Hub-Signature-256 header
        # matches. Without this, any caller can trigger a git fetch.
        # Configure the same value in your git host's webhook settings.
        # WEBHOOK_SECRET = ""

        # URL path for the webhook endpoint. Defaults to /webhook.
        # WEBHOOK_PATH = "/webhook"

        # ── JSON API ────────────────────────────────────────────────────────
        #
        # The /api/v1/ endpoints are disabled by default. Set API_KEY to a
        # long random string to enable them. All /api/v1/ requests must include:
        #   Authorization: Bearer <key>
        # The OpenAPI spec is served publicly at /api/openapi.json.
        # Store the key in a Nomad Variable rather than hardcoding it here.
        # API_KEY = "change-me"

        # ── Job selection ───────────────────────────────────────────────────
        #
        # nomad-botherer does not watch every job on the cluster by default.
        # A job must match at least one of the two criteria below.
        #
        # Meta key (on by default): any job with
        #   meta { gitops_managed = "true" }
        # in its HCL definition is automatically watched. The prefix before
        # "_managed" is controlled by MANAGED_META_PREFIX. If you need to
        # change it, keep "gitops" as a root (e.g. "gitops_myteam") so all
        # nomad-botherer keys remain visually grouped on a shared cluster.
        MANAGED_META_PREFIX = "gitops"

        # Glob: watch all jobs whose name matches a shell glob pattern.
        # Use "*" to watch every job in the namespace, or "production-*" to
        # watch all jobs whose names start with "production-". The glob and
        # the meta key are a union: a job is selected if it matches either.
        # Unset (empty string) means no glob selection.
        # JOB_SELECTOR_GLOB = ""

        # ── Diff behaviour ──────────────────────────────────────────────────

        # How often to compare Nomad state against the git repo, regardless of
        # whether a git push has occurred. Defaults to 1m.
        DIFF_INTERVAL = "1m"

        # By default, dead (stopped) Nomad jobs are treated as
        # missing_from_nomad — a job that was intentionally stopped is expected
        # to be absent. Set to "true" to compare dead jobs' specs against their
        # HCL definitions instead.
        # INCLUDE_DEAD_JOBS = "false"

        # ── Staleness guards ────────────────────────────────────────────────
        #
        # These settings force a refresh if the normal polling or webhook path
        # falls behind. Both default to 0 (disabled).
        #
        # Force a git fetch if the last successful fetch is older than this:
        # MAX_GIT_STALENESS = "30m"
        #
        # Force a diff check if the last successful check is older than this:
        # MAX_NOMAD_STALENESS = "10m"

        # ── Logging ─────────────────────────────────────────────────────────
        #
        # Structured JSON logs are written to stderr. Valid levels:
        # debug, info, warn, error. "info" is appropriate for production.
        LOG_LEVEL = "info"
      }

      # ── Reading secrets from Nomad Variables ──────────────────────────────
      #
      # Uncomment and adapt this block to pull GIT_TOKEN and API_KEY from
      # a Nomad Variable rather than hardcoding them in env {}.
      #
      # First, create the variable:
      #   nomad var put nomad/jobs/nomad-botherer \
      #     GIT_TOKEN=ghp_... \
      #     API_KEY=your-long-random-key
      #
      # template {
      #   data        = <<-EOT
      #     {{ with nomadVar "nomad/jobs/nomad-botherer" }}
      #     GIT_TOKEN={{ .GIT_TOKEN }}
      #     API_KEY={{ .API_KEY }}
      #     {{ end }}
      #   EOT
      #   destination = "secrets/env"
      #   env         = true
      # }

      resources {
        # nomad-botherer is lightweight: it holds a single in-memory git clone
        # and makes periodic API calls. 128 MiB is ample for most repos; raise
        # it if your repo is very large or contains many large binary files.
        cpu    = 100
        memory = 128
      }

      service {
        name     = "nomad-botherer"
        port     = "http"

        # Change to "consul" if you are using Consul service discovery.
        provider = "nomad"

        check {
          type     = "http"
          path     = "/healthz"
          interval = "15s"
          timeout  = "3s"
          # /healthz returns HTTP 503 while the initial git clone and first
          # diff check are in progress. This is normal on first deploy; the
          # check will pass once startup completes (typically 10–30 seconds).
        }
      }
    }
  }
}
