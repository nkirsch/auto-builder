#!/usr/bin/env bash
# Idempotent GCP setup for auto-builder. Safe to re-run.
set -euo pipefail

PROJECT=${PROJECT:-nkirsch-auto-builder}
REGION=${REGION:-us-central1}
BILLING=${BILLING:-} # billing account ID; needed only to link a new project
REPO=${REPO:-nkirsch/auto-builder}
DOMAIN=${DOMAIN:-builder.extreme.org}
SQL_INSTANCE=${SQL_INSTANCE:-builder}
SQL_DB=${SQL_DB:-builder}
SQL_USER=${SQL_USER:-builder}
AR_REPO=${AR_REPO:-builder}
BUCKET=${BUCKET:-$PROJECT-data}
SERVICE=${SERVICE:-builder}
JOB=${JOB:-builder-runner}
# Claude auth for the runner: the comma-separated ANTHROPIC_* IDs printed by
# kirsch/home infrastructure/claude-wif/provision.sh gcp (Workload Identity
# Federation); the job gets them as env and no API key. Defaults to rule
# auto-builder-runner-rule. Set it empty (CLAUDE_FEDERATION=) to mount ANTHROPIC_API_KEY instead.
CLAUDE_FEDERATION=${CLAUDE_FEDERATION-ANTHROPIC_ORGANIZATION_ID=dc5626e0-63a7-45a9-a418-b689f7ae6c40,ANTHROPIC_SERVICE_ACCOUNT_ID=svac_014ixSeHxixYtRESzvJZi7ah,ANTHROPIC_FEDERATION_RULE_ID=fdrl_01AUuCtDtwh5X21ynFaaaNQJ,ANTHROPIC_WORKSPACE_ID=wrkspc_018YcoPCmM7YfmUw7LC2hGzc}
KEY_SECRET=$([ -n "$CLAUDE_FEDERATION" ] || echo ANTHROPIC_API_KEY)

g() { gcloud --project "$PROJECT" --quiet "$@"; }
log() { echo "== $*"; }
# New service accounts take a while to become visible to IAM.
retry() { for _ in 1 2 3 4 5 6; do "$@" && return; sleep 10; done; return 1; }
sa() { echo "$1@$PROJECT.iam.gserviceaccount.com"; }

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
  storage.googleapis.com cloudbuild.googleapis.com sts.googleapis.com

log "cloud sql $SQL_INSTANCE"
g sql instances describe "$SQL_INSTANCE" >/dev/null 2>&1 ||
  g sql instances create "$SQL_INSTANCE" --region "$REGION" \
    --database-version POSTGRES_17 --edition enterprise --tier db-f1-micro \
    --storage-size 10 --storage-auto-increase
g sql databases describe "$SQL_DB" --instance "$SQL_INSTANCE" >/dev/null 2>&1 ||
  g sql databases create "$SQL_DB" --instance "$SQL_INSTANCE"

# Generated secrets; the DB password lives only inside DATABASE_URL.
secret() { # name value
  g secrets describe "$1" >/dev/null 2>&1 ||
    printf '%s' "$2" | g secrets create "$1" --replication-policy automatic --data-file -
}
if ! g secrets describe DATABASE_URL >/dev/null 2>&1; then
  pw=$(openssl rand -hex 24)
  g sql users create "$SQL_USER" --instance "$SQL_INSTANCE" --password "$pw" 2>/dev/null ||
    g sql users set-password "$SQL_USER" --instance "$SQL_INSTANCE" --password "$pw"
  secret DATABASE_URL "postgres://$SQL_USER:$pw@/$SQL_DB?host=/cloudsql/$PROJECT:$REGION:$SQL_INSTANCE"
fi
log "secrets"
secret SESSION_KEY "$(openssl rand -hex 32)"
secret RUNNER_TOKEN "$(openssl rand -hex 32)"
secret TICK_TOKEN "$(openssl rand -hex 32)"
secret STATS_TOKEN "$(openssl rand -hex 32)" # the hub's /admin/sites reads GET /api/stats with it
# Filled in by hand (see DESIGN.md manual steps); created empty-less so Cloud Run can mount them.
for s in GITHUB_CLIENT_ID GITHUB_CLIENT_SECRET GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET ANTHROPIC_API_KEY GH_APP_ID \
  GH_APP_INSTALLATION_ID GH_APP_PRIVATE_KEY PUSHOVER_TOKEN PUSHOVER_USER; do
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
SERVER_SECRETS="DATABASE_URL SESSION_KEY RUNNER_TOKEN TICK_TOKEN STATS_TOKEN GITHUB_CLIENT_ID GITHUB_CLIENT_SECRET GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET GH_APP_ID GH_APP_INSTALLATION_ID GH_APP_PRIVATE_KEY PUSHOVER_TOKEN PUSHOVER_USER"
# The runner gets GitHub tokens from the server: no GH_APP_* secrets.
RUNNER_SECRETS="RUNNER_TOKEN $KEY_SECRET"
APP_SECRETS="GH_APP_ID GH_APP_INSTALLATION_ID GH_APP_PRIVATE_KEY"
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
proj_role builder-server roles/cloudsql.client
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
WIF_CONDITION="assertion.repository == '$REPO' && assertion.ref == 'refs/heads/master'"
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
if ! g run services describe "$SERVICE" --region "$REGION" >/dev/null 2>&1; then
  # First deploy builds from source; CI owns deploys after this.
  g run deploy "$SERVICE" --region "$REGION" --source "$(dirname "$0")/.." \
    --service-account "$(sa builder-server)" --allow-unauthenticated \
    --add-cloudsql-instances "$PROJECT:$REGION:$SQL_INSTANCE" \
    --set-env-vars "BASE_URL=https://$DOMAIN,GCS_BUCKET=$BUCKET,GCP_PROJECT=$PROJECT,GCP_REGION=$REGION,RUNNER_JOB=$JOB,GIT_SHA=$(git -C "$(dirname "$0")" rev-parse HEAD)" \
    --set-secrets "$(mounts "$SERVER_SECRETS")" \
    --min-instances 0 --max-instances 2 --memory 512Mi
fi
# Services created before Google sign-in lack its secrets.
if ! g run services describe "$SERVICE" --region "$REGION" --format json | grep -q GOOGLE_CLIENT_ID; then
  g run services update "$SERVICE" --region "$REGION" \
    --update-secrets "$(mounts "GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET")" >/dev/null
fi
# ...and boards from before the hub's all-sites page lack STATS_TOKEN.
if ! g run services describe "$SERVICE" --region "$REGION" --format json | grep -q STATS_TOKEN; then
  g run services update "$SERVICE" --region "$REGION" --update-secrets "$(mounts STATS_TOKEN)" >/dev/null
fi

log "cloud run job $JOB"
if ! g run jobs describe "$JOB" --region "$REGION" >/dev/null 2>&1; then
  # Placeholder image until the deploy workflow pushes the runner.
  g run jobs create "$JOB" --region "$REGION" --image us-docker.pkg.dev/cloudrun/container/job:latest \
    --service-account "$(sa builder-runner)" --task-timeout 60m --max-retries 0 \
    --cpu 2 --memory 4Gi \
    --set-env-vars "BASE_URL=https://$DOMAIN${CLAUDE_FEDERATION:+,$CLAUDE_FEDERATION}" \
    --set-secrets "$(mounts "$RUNNER_SECRETS")"
fi
if [ -n "$CLAUDE_FEDERATION" ]; then
  g run jobs update "$JOB" --region "$REGION" --update-env-vars "$CLAUDE_FEDERATION" \
    --remove-secrets ANTHROPIC_API_KEY >/dev/null
fi
# Older jobs mounted the App secrets; unmount them, then revoke the runner's access.
g run jobs update "$JOB" --region "$REGION" --remove-secrets "${APP_SECRETS// /,}" >/dev/null
for s in $APP_SECRETS; do
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
g beta run domain-mappings describe --domain "$DOMAIN" --region "$REGION" >/dev/null 2>&1 ||
  g beta run domain-mappings create --service "$SERVICE" --domain "$DOMAIN" --region "$REGION" ||
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
  for v in "GCP_PROJECT=$PROJECT" "GCP_REGION=$REGION" \
    "GCP_WIF_PROVIDER=projects/$NUMBER/locations/global/workloadIdentityPools/github/providers/github" \
    "GCP_DEPLOYER=$(sa builder-deployer)" "IMAGE=$IMAGE"; do
    gh variable set "${v%%=*}" --repo "$REPO" --body "${v#*=}"
  done
fi

log "done"
