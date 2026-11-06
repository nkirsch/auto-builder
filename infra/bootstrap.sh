#!/usr/bin/env bash
# Idempotent GCP setup for auto-builder. Safe to re-run. Personal settings
# (Claude IDs, home host) aren't in code: pass them on the first run; later
# runs read them back from the runner job and secret HOME_HOST.
set -euo pipefail

PROJECT=${PROJECT:-nkirsch-auto-builder}
REGION=${REGION:-us-central1}
BILLING=${BILLING:-} # billing account ID; needed only to link a new project
REPO=${REPO:-nkirsch/auto-builder}
DOMAIN=${DOMAIN:-builder.extreme.org}
SIGNAL_URL=${SIGNAL_URL:-https://signal.nick.org} # admin pushes (internal/notify); the server account must be in signal's SIGNAL_GOOGLE
# The hub service sits by its database; jobs, images and apps stay in $REGION.
HUB_REGION=${HUB_REGION:-us-west1}
# Shared instance: one database + role per hub, each role unprivileged.
SQL_PROJECT=${SQL_PROJECT:-nick-org-2019}
SQL_REGION=${SQL_REGION:-us-west1}
SQL_INSTANCE=${SQL_INSTANCE:-nick-org}
SQL_CONN=$SQL_PROJECT:$SQL_REGION:$SQL_INSTANCE
SQL_DB=${SQL_DB:-builder}
SQL_USER=${SQL_USER:-builder}
AR_REPO=${AR_REPO:-builder}
BUCKET=${BUCKET:-$PROJECT-data}
SERVICE=${SERVICE:-builder}
JOB=${JOB:-builder-runner}
# Claude auth for the runner: the comma-separated ANTHROPIC_* IDs of a
# Workload Identity Federation rule; the job gets them as env and no API key.
# Empty keeps the runner job's; with none there either, the job mounts ANTHROPIC_API_KEY.
CLAUDE_FEDERATION=${CLAUDE_FEDERATION-}
# A DNS name (dynamic DNS) whose IPs' visits /logs leaves out, kept in secret
# HOME_HOST; empty keeps the secret's value.
HOME_HOST=${HOME_HOST-}
# Reporting spend to a hub (TENANCY.md): its URL and this board's token, kept
# in secret SPEND_REPORT_TOKEN. Empty keeps the service's or the secret's.
SPEND_REPORT_TO=${SPEND_REPORT_TO-}
SPEND_REPORT_TOKEN=${SPEND_REPORT_TOKEN-}
# DEPLOY_ON from app.env over app.defaults.env, as the workflows read it.
DEPLOY_ON=$(cd "$(dirname "$0")/.." && awk -F= '/^DEPLOY_ON=/ {v=$2} END {print v}' app.defaults.env app.env 2>/dev/null)

g() { gcloud --project "$PROJECT" --quiet "$@"; }
gs() { gcloud --project "$SQL_PROJECT" --quiet "$@"; }
log() { echo "== $*"; }
# New service accounts take a while to become visible to IAM.
retry() { for _ in 1 2 3 4 5 6; do "$@" && return; sleep 10; done; return 1; }
sa() { echo "$1@$PROJECT.iam.gserviceaccount.com"; }
run_env() { # services|jobs NAME PREFIX: NAME's plain env vars starting with PREFIX, as A=1,B=2
  g run "$1" describe "$2" --region "$REGION" --format json 2>/dev/null | python3 -c '
import json, sys
try:
    spec = json.load(sys.stdin)["spec"]["template"]["spec"]
except ValueError:
    sys.exit()
spec = spec.get("template", {}).get("spec", spec)  # a job nests one more template
print(",".join("%s=%s" % (e["name"], e["value"]) for c in spec.get("containers", [])
    for e in c.get("env", []) if e["name"].startswith(sys.argv[1]) and "value" in e))' "$3"
}
CLAUDE_FEDERATION=${CLAUDE_FEDERATION:-$(run_env jobs "$JOB" ANTHROPIC_)}
KEY_SECRET=$([ -n "$CLAUDE_FEDERATION" ] || echo ANTHROPIC_API_KEY)

log "project $PROJECT"
gcloud projects describe "$PROJECT" >/dev/null 2>&1 ||
  gcloud projects create "$PROJECT" --name auto-builder
if [ "$(gcloud billing projects describe "$PROJECT" --format 'value(billingEnabled)')" != True ]; then
  [ -n "$BILLING" ] || { echo "set BILLING=<billing account ID> to link $PROJECT" >&2; exit 1; }
  gcloud billing projects link "$PROJECT" --billing-account "$BILLING" >/dev/null
fi
NUMBER=$(gcloud projects describe "$PROJECT" --format 'value(projectNumber)')

log "apis"
g services enable run.googleapis.com sqladmin.googleapis.com \
  artifactregistry.googleapis.com secretmanager.googleapis.com \
  cloudscheduler.googleapis.com iamcredentials.googleapis.com \
  storage.googleapis.com cloudbuild.googleapis.com sts.googleapis.com \
  gmail.googleapis.com

log "cloud sql $SQL_CONN / $SQL_DB"
if ! gs sql instances describe "$SQL_INSTANCE" >/dev/null 2>&1; then
  [ "$SQL_PROJECT" = "$PROJECT" ] || { echo "no instance $SQL_CONN" >&2; exit 1; }
  gs sql instances create "$SQL_INSTANCE" --region "$SQL_REGION" \
    --database-version POSTGRES_17 --edition enterprise --tier db-f1-micro \
    --storage-size 10 --storage-auto-increase
fi
until [ "$(gs sql instances describe "$SQL_INSTANCE" --format 'value(state)')" = RUNNABLE ]; do sleep 15; done
# sql STATEMENTS — run as postgres through cloud-sql-proxy (brew install cloud-sql-proxy).
# The postgres password is reset to a throwaway value each time.
sql() {
  local port=54329 pw
  pw=$(openssl rand -hex 24)
  gs sql users set-password postgres --instance "$SQL_INSTANCE" --password "$pw" >/dev/null
  cloud-sql-proxy --gcloud-auth --port "$port" "$SQL_CONN" >/dev/null 2>&1 &
  local proxy=$!
  for _ in $(seq 30); do pg_isready -q -h 127.0.0.1 -p "$port" && break; sleep 1; done
  local rc=0
  PGPASSWORD=$pw psql -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$port" -U postgres -d postgres -c "$1" || rc=$?
  kill "$proxy"
  return $rc
}
# Generated secrets; the DB password lives only inside DATABASE_URL.
secret() { # name value
  g secrets describe "$1" >/dev/null 2>&1 ||
    printf '%s' "$2" | g secrets create "$1" --replication-policy automatic --data-file -
}
# Role and database are made in SQL, not gcloud: gcloud users join cloudsqlsuperuser,
# which owns every gcloud-made database, so apps could read each other's data.
if ! g secrets describe "DATABASE_URL" >/dev/null 2>&1; then
  pw=$(openssl rand -hex 24)
  sql "DO \$\$BEGIN
         IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$SQL_USER') THEN CREATE ROLE $SQL_USER LOGIN; END IF;
       END\$\$;
       ALTER ROLE $SQL_USER PASSWORD '$pw';"
  secret DATABASE_URL "postgres://$SQL_USER:$pw@/$SQL_DB?host=/cloudsql/$SQL_CONN"
fi
# postgres administers the role but needs SET to hand it a database.
sql "GRANT $SQL_USER TO postgres WITH INHERIT FALSE, SET TRUE"
gs sql databases describe "$SQL_DB" --instance "$SQL_INSTANCE" >/dev/null 2>&1 ||
  sql "CREATE DATABASE $SQL_DB OWNER $SQL_USER"
# Only the owner can revoke PUBLIC's CONNECT; from postgres, REVOKE does nothing.
sql "SET ROLE $SQL_USER;
     REVOKE ALL ON DATABASE $SQL_DB FROM PUBLIC;
     GRANT CONNECT ON DATABASE $SQL_DB TO postgres"
log "secrets"
secret SESSION_KEY "$(openssl rand -hex 32)"
secret TICK_TOKEN "$(openssl rand -hex 32)"
secret STATS_TOKEN "$(openssl rand -hex 32)" # the hub's Apps page reads GET /api/stats with it
# Filled in by hand (see DESIGN.md manual steps); created empty-less so Cloud Run can mount them.
for s in GITHUB_CLIENT_ID GITHUB_CLIENT_SECRET GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET ANTHROPIC_API_KEY GH_APP_ID \
  GH_APP_INSTALLATION_ID GH_APP_PRIVATE_KEY; do
  secret "$s" "unset"
done

log "artifact registry $AR_REPO"
g artifacts repositories describe "$AR_REPO" --location "$REGION" >/dev/null 2>&1 ||
  g artifacts repositories create "$AR_REPO" --location "$REGION" --repository-format docker

log "bucket $BUCKET"
gcloud storage buckets describe "gs://$BUCKET" >/dev/null 2>&1 ||
  gcloud storage buckets create "gs://$BUCKET" --project "$PROJECT" --location "$REGION" \
    --uniform-bucket-level-access --public-access-prevention

log "service accounts"
for a in builder-server builder-runner builder-deployer; do
  g iam service-accounts describe "$(sa $a)" >/dev/null 2>&1 ||
    g iam service-accounts create "$a"
done
# Least privilege: each SA gets only the roles and secrets it uses, scoped to
# the resource where IAM allows it. Older broad grants are removed on re-run.
SERVER_SECRETS="DATABASE_URL SESSION_KEY TICK_TOKEN STATS_TOKEN GITHUB_CLIENT_ID GITHUB_CLIENT_SECRET GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET GH_APP_ID GH_APP_INSTALLATION_ID GH_APP_PRIVATE_KEY"
if [ -n "$HOME_HOST" ]; then
  if ! g secrets describe HOME_HOST >/dev/null 2>&1; then
    printf '%s' "$HOME_HOST" | g secrets create HOME_HOST --replication-policy automatic --data-file -
  elif [ "$(g secrets versions access latest --secret HOME_HOST)" != "$HOME_HOST" ]; then
    printf '%s' "$HOME_HOST" | g secrets versions add HOME_HOST --data-file - >/dev/null
  fi
fi
if g secrets describe HOME_HOST >/dev/null 2>&1; then SERVER_SECRETS="$SERVER_SECRETS HOME_HOST"; fi
if [ -n "$SPEND_REPORT_TOKEN" ]; then
  if ! g secrets describe SPEND_REPORT_TOKEN >/dev/null 2>&1; then
    printf '%s' "$SPEND_REPORT_TOKEN" | g secrets create SPEND_REPORT_TOKEN --replication-policy automatic --data-file -
  elif [ "$(g secrets versions access latest --secret SPEND_REPORT_TOKEN)" != "$SPEND_REPORT_TOKEN" ]; then
    printf '%s' "$SPEND_REPORT_TOKEN" | g secrets versions add SPEND_REPORT_TOKEN --data-file - >/dev/null
  fi
fi
if g secrets describe SPEND_REPORT_TOKEN >/dev/null 2>&1; then SERVER_SECRETS="$SERVER_SECRETS SPEND_REPORT_TOKEN"; fi
# The runner gets GitHub tokens and its RUN_TOKEN from the server: no GH_APP_*
# secrets, and no RUNNER_TOKEN (a shared bearer older runners used).
RUNNER_SECRETS="$KEY_SECRET"
APP_SECRETS="GH_APP_ID GH_APP_INSTALLATION_ID GH_APP_PRIVATE_KEY"
OLD_RUNNER_SECRETS="$APP_SECRETS RUNNER_TOKEN"
mounts() { for s in $1; do printf '%s=%s:latest,' "$s" "$s"; done | sed 's/,$//'; }
proj_role() { retry g projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:$(sa $1)" \
  --role "$2" --condition None >/dev/null; }
drop_proj_role() { g projects remove-iam-policy-binding "$PROJECT" --member "serviceAccount:$(sa $1)" \
  --role "$2" --condition None >/dev/null 2>&1 || true; }
secret_access() { # sa secrets
  for s in $2; do
    retry g secrets add-iam-policy-binding "$s" --member "serviceAccount:$(sa $1)" \
      --role roles/secretmanager.secretAccessor >/dev/null
  done
}
retry gs projects add-iam-policy-binding "$SQL_PROJECT" --member "serviceAccount:$(sa builder-server)" \
  --role roles/cloudsql.client --condition None >/dev/null
secret_access builder-server "$SERVER_SECRETS"
# The runner executes the agent on untrusted item text: no DATABASE_URL,
# SESSION_KEY, TICK_TOKEN or OAuth secret, and no other GCP access.
secret_access builder-runner "$RUNNER_SECRETS"
drop_proj_role builder-server roles/secretmanager.secretAccessor
drop_proj_role builder-runner roles/secretmanager.secretAccessor
# Deploys only update the existing service and job (no IAM changes): run.developer.
proj_role builder-deployer roles/run.developer
drop_proj_role builder-deployer roles/run.admin
retry g artifacts repositories add-iam-policy-binding "$AR_REPO" --location "$REGION" \
  --member "serviceAccount:$(sa builder-deployer)" --role roles/artifactregistry.writer >/dev/null
drop_proj_role builder-deployer roles/artifactregistry.writer
# The server only writes and reads objects.
retry gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" \
  --member "serviceAccount:$(sa builder-server)" --role roles/storage.objectUser >/dev/null
gcloud storage buckets remove-iam-policy-binding "gs://$BUCKET" \
  --member "serviceAccount:$(sa builder-server)" --role roles/storage.objectAdmin >/dev/null 2>&1 || true
# Deployer deploys as the server/runner SAs.
for a in builder-server builder-runner; do
  retry g iam service-accounts add-iam-policy-binding "$(sa $a)" \
    --member "serviceAccount:$(sa builder-deployer)" --role roles/iam.serviceAccountUser >/dev/null
done

log "workload identity for $REPO"
g iam workload-identity-pools describe github --location global >/dev/null 2>&1 ||
  g iam workload-identity-pools create github --location global --display-name GitHub
# Only master (the deploy workflow) may impersonate the deployer: the agent
# pushes branches to this repo, and a workflow on one must not reach GCP.
# DEPLOY_ON=release adds v* tags; deploy.yml refuses a tag not on master.
WIF_REF="assertion.ref == 'refs/heads/master'"
[ "$DEPLOY_ON" = release ] && WIF_REF="($WIF_REF || assertion.ref.startsWith('refs/tags/v'))"
WIF_CONDITION="assertion.repository == '$REPO' && $WIF_REF"
if g iam workload-identity-pools providers describe github --location global \
  --workload-identity-pool github >/dev/null 2>&1; then
  g iam workload-identity-pools providers update-oidc github --location global \
    --workload-identity-pool github --attribute-condition "$WIF_CONDITION" >/dev/null
else
  g iam workload-identity-pools providers create-oidc github --location global \
    --workload-identity-pool github --issuer-uri https://token.actions.githubusercontent.com \
    --attribute-mapping 'google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.ref=assertion.ref' \
    --attribute-condition "$WIF_CONDITION"
fi
retry g iam service-accounts add-iam-policy-binding "$(sa builder-deployer)" \
  --role roles/iam.workloadIdentityUser \
  --member "principalSet://iam.googleapis.com/projects/$NUMBER/locations/global/workloadIdentityPools/github/attribute.repository/$REPO" >/dev/null

IMAGE="$REGION-docker.pkg.dev/$PROJECT/$AR_REPO"
log "cloud run service $SERVICE"
if ! g run services describe "$SERVICE" --region "$HUB_REGION" >/dev/null 2>&1; then
  # First deploy builds from source; CI owns deploys after this.
  g run deploy "$SERVICE" --region "$HUB_REGION" --source "$(dirname "$0")/.." \
    --service-account "$(sa builder-server)" --allow-unauthenticated \
    --add-cloudsql-instances "$SQL_CONN" \
    --set-env-vars "BASE_URL=https://$DOMAIN,SIGNAL_URL=$SIGNAL_URL,GCS_BUCKET=$BUCKET,GCP_PROJECT=$PROJECT,GCP_REGION=$REGION,RUNNER_JOB=$JOB,GIT_SHA=$(git -C "$(dirname "$0")" rev-parse HEAD)" \
    --set-secrets "$(mounts "$SERVER_SECRETS")" \
    --min-instances 0 --max-instances 2 --memory 512Mi
fi
# Services created before Google sign-in lack its secrets.
if ! g run services describe "$SERVICE" --region "$HUB_REGION" --format json | grep -q GOOGLE_CLIENT_ID; then
  g run services update "$SERVICE" --region "$HUB_REGION" \
    --update-secrets "$(mounts "GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET")" >/dev/null
fi
# ...as do boards from before HOME_HOST, once it's set...
if [[ " $SERVER_SECRETS " == *" HOME_HOST "* ]] && ! g run services describe "$SERVICE" --region "$HUB_REGION" --format json | grep -q '"HOME_HOST"'; then
  g run services update "$SERVICE" --region "$HUB_REGION" --update-secrets "$(mounts HOME_HOST)" >/dev/null
fi
# ...or reporting spend, once it's set...
if [[ " $SERVER_SECRETS " == *" SPEND_REPORT_TOKEN "* ]]; then
  g run services update "$SERVICE" --region "$HUB_REGION" --update-secrets "$(mounts SPEND_REPORT_TOKEN)" \
    ${SPEND_REPORT_TO:+--update-env-vars "SPEND_REPORT_TO=$SPEND_REPORT_TO"} >/dev/null
fi
# ...and boards from before the hub's all-sites page lack STATS_TOKEN.
if ! g run services describe "$SERVICE" --region "$HUB_REGION" --format json | grep -q STATS_TOKEN; then
  g run services update "$SERVICE" --region "$HUB_REGION" --update-secrets "$(mounts STATS_TOKEN)" >/dev/null
fi

log "cloud run job $JOB"
if ! g run jobs describe "$JOB" --region "$REGION" >/dev/null 2>&1; then
  # Placeholder image until the deploy workflow pushes the runner.
  g run jobs create "$JOB" --region "$REGION" --image us-docker.pkg.dev/cloudrun/container/job:latest \
    --service-account "$(sa builder-runner)" --task-timeout 60m --max-retries 0 \
    --cpu 2 --memory 4Gi \
    --set-env-vars "BASE_URL=https://$DOMAIN${CLAUDE_FEDERATION:+,$CLAUDE_FEDERATION}" \
    ${RUNNER_SECRETS:+--set-secrets "$(mounts "$RUNNER_SECRETS")"}
fi
if [ -n "$CLAUDE_FEDERATION" ]; then
  g run jobs update "$JOB" --region "$REGION" --update-env-vars "$CLAUDE_FEDERATION" \
    --remove-secrets ANTHROPIC_API_KEY >/dev/null
fi
# Older jobs mounted the App secrets and RUNNER_TOKEN; unmount them, then
# revoke the runner's access. The server no longer reads RUNNER_TOKEN either.
g run jobs update "$JOB" --region "$REGION" --remove-secrets "${OLD_RUNNER_SECRETS// /,}" >/dev/null
if g run services describe "$SERVICE" --region "$HUB_REGION" --format json | grep -q '"RUNNER_TOKEN"'; then
  g run services update "$SERVICE" --region "$HUB_REGION" --remove-secrets RUNNER_TOKEN >/dev/null
fi
for s in $OLD_RUNNER_SECRETS; do
  g secrets remove-iam-policy-binding "$s" --member "serviceAccount:$(sa builder-runner)" \
    --role roles/secretmanager.secretAccessor >/dev/null 2>&1 || true
done
# Server launches the job with a RUN_ID override.
retry g run jobs add-iam-policy-binding "$JOB" --region "$REGION" \
  --member "serviceAccount:$(sa builder-server)" --role roles/run.developer >/dev/null
retry g iam service-accounts add-iam-policy-binding "$(sa builder-runner)" \
  --member "serviceAccount:$(sa builder-server)" --role roles/iam.serviceAccountUser >/dev/null

log "scheduler tick"
if ! g scheduler jobs describe builder-tick --location "$REGION" >/dev/null 2>&1; then
  tick=$(g secrets versions access latest --secret TICK_TOKEN)
  g scheduler jobs create http builder-tick --location "$REGION" --schedule '*/10 * * * *' \
    --uri "https://$DOMAIN/api/gate/tick" --http-method POST \
    --headers "Authorization=Bearer $tick"
fi

log "domain mapping $DOMAIN"
g beta run domain-mappings describe --domain "$DOMAIN" --region "$HUB_REGION" >/dev/null 2>&1 ||
  g beta run domain-mappings create --service "$SERVICE" --domain "$DOMAIN" --region "$HUB_REGION" ||
  echo "!! domain mapping failed: verify the domain for this account (DESIGN.md manual step 2)"

log "github repo settings"
if gh auth status >/dev/null 2>&1; then
  gh api -X PATCH "repos/$REPO" -F allow_auto_merge=true -F allow_squash_merge=true \
    -F delete_branch_on_merge=true >/dev/null
  # GitHub follows branch renames, so check the name that comes back.
  if [ "$(gh api "repos/$REPO/branches/master" --jq .name 2>/dev/null)" = master ]; then
    gh api -X PUT "repos/$REPO/branches/master/protection" --input - >/dev/null <<JSON
{"required_status_checks":{"strict":false,"contexts":["test"]},"enforce_admins":false,
 "required_pull_request_reviews":null,"restrictions":null}
JSON
  else
    echo "!! no master branch yet; re-run after it exists to protect it"
  fi
  for v in "GCP_PROJECT=$PROJECT" "GCP_REGION=$REGION" "HUB_REGION=$HUB_REGION" \
    "GCP_WIF_PROVIDER=projects/$NUMBER/locations/global/workloadIdentityPools/github/providers/github" \
    "GCP_DEPLOYER=$(sa builder-deployer)" "IMAGE=$IMAGE"; do
    gh variable set "${v%%=*}" --repo "$REPO" --body "${v#*=}"
  done
fi

log "done"
