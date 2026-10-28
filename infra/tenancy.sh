#!/usr/bin/env bash
# Idempotent GCP + GitHub setup for multi-tenant mode (TENANCY.md): the hub
# service, the shared runner and provision jobs, their accounts and secrets,
# Workload Identity for app repos and the platform repo, the tick, and the
# apex domain. Safe to re-run. Defaults are extreme.org's.
set -euo pipefail

PROJECT=${PROJECT:-nkirsch-auto-builder}
REGION=${REGION:-us-central1}
NAME=${NAME:-extreme}                  # prefixes every platform resource and secret
DOMAIN=${DOMAIN:-extreme.org}          # hub at the apex, apps on <slug>.$DOMAIN
ZONE=${ZONE:-extreme-org}              # Cloud DNS zone for $DOMAIN
HUB_ALIASES=${HUB_ALIASES-hub.$DOMAIN}  # space-separated hosts in $DOMAIN that 308 to the hub
ORG=${ORG:-extreme-dot-org}            # GitHub org holding the app repos
PLATFORM_REPO=${PLATFORM_REPO:-extreme-dot-org/extreme}
SQL_INSTANCE=${SQL_INSTANCE:-builder}
ADMIN_LOGIN=${ADMIN_LOGIN:-nkirsch}
ADMIN_EMAIL=${ADMIN_EMAIL:-nick@kirsch.org}
# Single-board sites on the hub's /admin/sites page (space-separated name=board URL);
# each mounts the shared STATS_TOKEN secret, which this project holds.
SITES=${SITES-builder=https://builder.extreme.org momo=https://momo.kirsch.org/iterate katrina=https://katrina.kirsch.org/iterate youkoh.org=https://youkoh.kirsch.org/iterate}
# Claude auth for the runner: the comma-separated ANTHROPIC_* IDs printed by
# kirsch/home infrastructure/claude-wif/provision.sh gcp (Workload Identity
# Federation); the job gets them as env and no API key. Defaults to rule
# extreme-runner-rule. Set it empty (CLAUDE_FEDERATION=) to mount ANTHROPIC_API_KEY instead.
CLAUDE_FEDERATION=${CLAUDE_FEDERATION-ANTHROPIC_ORGANIZATION_ID=dc5626e0-63a7-45a9-a418-b689f7ae6c40,ANTHROPIC_SERVICE_ACCOUNT_ID=svac_011UwfAzct6jMWhqJGcAGALe,ANTHROPIC_FEDERATION_RULE_ID=fdrl_01B67Tae2MQhWtk3er4XMjTr,ANTHROPIC_WORKSPACE_ID=wrkspc_01A9j1vRx1NkpFzRfDx7aTTd}
KEY_SECRET=$([ -n "$CLAUDE_FEDERATION" ] || echo ANTHROPIC_API_KEY)

g() { gcloud --project "$PROJECT" --quiet "$@"; }
log() { echo "== $*"; }
retry() { for _ in 1 2 3 4 5 6; do "$@" && return; sleep 10; done; return 1; }
sa() { echo "$NAME-$1@$PROJECT.iam.gserviceaccount.com"; }
NUMBER=$(gcloud projects describe "$PROJECT" --format 'value(projectNumber)')

log "apis"
g services enable run.googleapis.com sqladmin.googleapis.com artifactregistry.googleapis.com \
  secretmanager.googleapis.com cloudscheduler.googleapis.com iamcredentials.googleapis.com \
  sts.googleapis.com dns.googleapis.com storage.googleapis.com cloudresourcemanager.googleapis.com \
  gmail.googleapis.com

secret() { # name value: create once; later runs keep the value
  g secrets describe "$1" >/dev/null 2>&1 ||
    printf '%s' "$2" | g secrets create "$1" --replication-policy automatic --data-file -
}

log "database $NAME on $SQL_INSTANCE"
g sql databases describe "$NAME" --instance "$SQL_INSTANCE" >/dev/null 2>&1 ||
  g sql databases create "$NAME" --instance "$SQL_INSTANCE"
if ! g secrets describe "$NAME-DATABASE_URL" >/dev/null 2>&1; then
  pw=$(openssl rand -hex 24)
  g sql users create "$NAME" --instance "$SQL_INSTANCE" --password "$pw" 2>/dev/null ||
    g sql users set-password "$NAME" --instance "$SQL_INSTANCE" --password "$pw"
  secret "$NAME-DATABASE_URL" "postgres://$NAME:$pw@/$NAME?host=/cloudsql/$PROJECT:$REGION:$SQL_INSTANCE"
fi

log "secrets"
secret "$NAME-SESSION_KEY" "$(openssl rand -hex 32)"
secret "$NAME-TICK_TOKEN" "$(openssl rand -hex 32)"
secret STATS_TOKEN "$(openssl rand -hex 32)" # shared with bootstrap.sh and the single-board sites
# Filled in by hand (TENANCY.md manual steps).
for s in GITHUB_CLIENT_ID GITHUB_CLIENT_SECRET GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET \
  GH_APP_ID GH_APP_INSTALLATION_ID GH_APP_PRIVATE_KEY $KEY_SECRET; do
  secret "$NAME-$s" unset
done
# mounts "ENV=SECRET ..." → ENV=SECRET:latest,...
mounts() { for m in $1; do printf '%s:latest,' "$m"; done | sed 's/,$//'; }
prefixed() { for s in $1; do printf '%s=%s ' "$s" "$NAME-$s"; done; }
APP_SECRETS="GH_APP_ID GH_APP_INSTALLATION_ID GH_APP_PRIVATE_KEY"
SERVER_MOUNTS="$(prefixed "DATABASE_URL SESSION_KEY TICK_TOKEN GITHUB_CLIENT_ID GITHUB_CLIENT_SECRET GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET $APP_SECRETS") PUSHOVER_TOKEN=PUSHOVER_TOKEN PUSHOVER_USER=PUSHOVER_USER STATS_TOKEN=STATS_TOKEN"
RUNNER_MOUNTS=$(prefixed "$KEY_SECRET")
PROVISION_MOUNTS="$(prefixed "DATABASE_URL $APP_SECRETS")"

log "artifact registry $NAME, bucket $PROJECT-$NAME"
g artifacts repositories describe "$NAME" --location "$REGION" >/dev/null 2>&1 ||
  g artifacts repositories create "$NAME" --location "$REGION" --repository-format docker
gcloud storage buckets describe "gs://$PROJECT-$NAME" >/dev/null 2>&1 ||
  gcloud storage buckets create "gs://$PROJECT-$NAME" --project "$PROJECT" --location "$REGION" \
    --uniform-bucket-level-access --public-access-prevention

log "service accounts"
for a in server runner deployer provisioner; do
  g iam service-accounts describe "$(sa $a)" >/dev/null 2>&1 || g iam service-accounts create "$NAME-$a"
done
proj_role() { retry g projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:$(sa $1)" \
  --role "$2" --condition None >/dev/null; }
secret_access() { # account "ENV=SECRET ..."
  for m in $2; do
    retry g secrets add-iam-policy-binding "${m#*=}" --member "serviceAccount:$(sa $1)" \
      --role roles/secretmanager.secretAccessor >/dev/null
  done
}
act_as() { # who whom
  retry g iam service-accounts add-iam-policy-binding "$(sa $2)" \
    --member "serviceAccount:$(sa $1)" --role roles/iam.serviceAccountUser >/dev/null
}
# The hub: its database and secrets, its bucket, starting both jobs, and
# reading the apps' services to mark merges shipped.
proj_role server roles/cloudsql.client
proj_role server roles/run.viewer
secret_access server "$SERVER_MOUNTS"
retry gcloud storage buckets add-iam-policy-binding "gs://$PROJECT-$NAME" \
  --member "serviceAccount:$(sa server)" --role roles/storage.objectUser >/dev/null
act_as server runner
act_as server provisioner
# The runner runs the agent on untrusted text: only the Anthropic key.
secret_access runner "$RUNNER_MOUNTS"
# The provisioner creates each app's resources; the internet-facing hub never
# holds these roles.
for r in roles/run.admin roles/iam.serviceAccountAdmin roles/iam.serviceAccountUser \
  roles/artifactregistry.admin roles/dns.admin roles/cloudsql.client; do
  proj_role provisioner "$r"
done
secret_access provisioner "$PROVISION_MOUNTS"
# Deploys update the existing service and jobs as their accounts.
retry g artifacts repositories add-iam-policy-binding "$NAME" --location "$REGION" \
  --member "serviceAccount:$(sa deployer)" --role roles/artifactregistry.writer >/dev/null
for a in server runner provisioner; do act_as deployer "$a"; done

log "workload identity pool $NAME"
g iam workload-identity-pools describe "$NAME" --location global >/dev/null 2>&1 ||
  g iam workload-identity-pools create "$NAME" --location global --display-name "$NAME"
provider() { # id condition: only master may deploy; the agent pushes branches
  if g iam workload-identity-pools providers describe "$1" --location global --workload-identity-pool "$NAME" >/dev/null 2>&1; then
    g iam workload-identity-pools providers update-oidc "$1" --location global \
      --workload-identity-pool "$NAME" --attribute-condition "$2" >/dev/null
  else
    g iam workload-identity-pools providers create-oidc "$1" --location global \
      --workload-identity-pool "$NAME" --issuer-uri https://token.actions.githubusercontent.com \
      --attribute-mapping 'google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.ref=assertion.ref' \
      --attribute-condition "$2"
  fi
}
# App repos (the provisioner binds each app's deployer to its own repo).
provider github "assertion.repository_owner == '$ORG' && assertion.ref == 'refs/heads/master'"
# The platform repo deploys the hub and jobs.
provider platform "assertion.repository == '$PLATFORM_REPO' && assertion.ref == 'refs/heads/master'"
retry g iam service-accounts add-iam-policy-binding "$(sa deployer)" --role roles/iam.workloadIdentityUser \
  --member "principalSet://iam.googleapis.com/projects/$NUMBER/locations/global/workloadIdentityPools/$NAME/attribute.repository/$PLATFORM_REPO" >/dev/null

# Placeholders until the platform repo's deploy pushes real images; deploys
# change only the image and a few env vars, so this config persists.
log "cloud run service $NAME"
if ! g run services describe "$NAME" --region "$REGION" >/dev/null 2>&1; then
  g run deploy "$NAME" --region "$REGION" --image us-docker.pkg.dev/cloudrun/container/hello \
    --service-account "$(sa server)" --allow-unauthenticated \
    --add-cloudsql-instances "$PROJECT:$REGION:$SQL_INSTANCE" \
    --set-env-vars "TENANCY=on,REPO=$PLATFORM_REPO,TENANT_ORG=$ORG,BASE_URL=https://$DOMAIN,GCS_BUCKET=$PROJECT-$NAME,GCP_PROJECT=$PROJECT,GCP_REGION=$REGION,RUNNER_JOB=$NAME-runner,PROVISION_JOB=$NAME-provision,ADMIN_LOGIN=$ADMIN_LOGIN,ADMIN_EMAIL=$ADMIN_EMAIL,ISSUE_SYNC=on" \
    --set-secrets "$(mounts "$SERVER_MOUNTS")" \
    --min-instances 0 --max-instances 2 --memory 512Mi
fi
# Hubs created before the all-sites page and aliases lack these; ^;^ keeps SITES' spaces and REDIRECT_HOSTS' commas.
aliases=$(echo $HUB_ALIASES | tr ' ' ,)
g run services update "$NAME" --region "$REGION" \
  --update-env-vars "^;^SITES=$SITES;APP_DOMAIN=$DOMAIN;REDIRECT_HOSTS=$aliases" \
  --update-secrets "STATS_TOKEN=STATS_TOKEN:latest" >/dev/null
retry g run services add-iam-policy-binding "$NAME" --region "$REGION" \
  --member "serviceAccount:$(sa deployer)" --role roles/run.developer >/dev/null

log "jobs $NAME-runner, $NAME-provision"
if ! g run jobs describe "$NAME-runner" --region "$REGION" >/dev/null 2>&1; then
  g run jobs create "$NAME-runner" --region "$REGION" --image us-docker.pkg.dev/cloudrun/container/job:latest \
    --service-account "$(sa runner)" --task-timeout 60m --max-retries 0 --cpu 2 --memory 4Gi \
    --set-env-vars "BASE_URL=https://$DOMAIN${CLAUDE_FEDERATION:+,$CLAUDE_FEDERATION}" \
    ${RUNNER_MOUNTS:+--set-secrets "$(mounts "$RUNNER_MOUNTS")"}
fi
if [ -n "$CLAUDE_FEDERATION" ]; then
  g run jobs update "$NAME-runner" --region "$REGION" --update-env-vars "$CLAUDE_FEDERATION" \
    --remove-secrets ANTHROPIC_API_KEY >/dev/null
fi
if ! g run jobs describe "$NAME-provision" --region "$REGION" >/dev/null 2>&1; then
  g run jobs create "$NAME-provision" --region "$REGION" --image us-docker.pkg.dev/cloudrun/container/job:latest \
    --service-account "$(sa provisioner)" --task-timeout 30m --max-retries 0 \
    --set-cloudsql-instances "$PROJECT:$REGION:$SQL_INSTANCE" \
    --set-env-vars "GCP_PROJECT=$PROJECT,GCP_REGION=$REGION,TENANT_ORG=$ORG,APP_DOMAIN=$DOMAIN,DNS_ZONE=$ZONE,WIF_POOL=$NAME,WIF_PROVIDER=github,PLATFORM_REPO=$PLATFORM_REPO,UPSTREAM=${UPSTREAM:-nkirsch/auto-builder}" \
    --set-secrets "$(mounts "$PROVISION_MOUNTS")"
fi
for j in runner provision; do
  # The hub starts both with env overrides; deploys update their images.
  retry g run jobs add-iam-policy-binding "$NAME-$j" --region "$REGION" \
    --member "serviceAccount:$(sa server)" --role roles/run.developer >/dev/null
  retry g run jobs add-iam-policy-binding "$NAME-$j" --region "$REGION" \
    --member "serviceAccount:$(sa deployer)" --role roles/run.developer >/dev/null
done

log "scheduler tick"
if ! g scheduler jobs describe "$NAME-tick" --location "$REGION" >/dev/null 2>&1; then
  tick=$(g secrets versions access latest --secret "$NAME-TICK_TOKEN")
  g scheduler jobs create http "$NAME-tick" --location "$REGION" --schedule '*/10 * * * *' \
    --uri "https://$DOMAIN/api/gate/tick" --http-method POST --headers "Authorization=Bearer $tick"
fi

log "domain $DOMAIN"
g beta run domain-mappings describe --domain "$DOMAIN" --region "$REGION" >/dev/null 2>&1 ||
  g beta run domain-mappings create --service "$NAME" --domain "$DOMAIN" --region "$REGION"
# The apex can't be a CNAME: copy the mapping's A/AAAA records into the zone.
for type in A AAAA; do
  rr=$(g beta run domain-mappings describe --domain "$DOMAIN" --region "$REGION" \
    --format json | jq -r --arg t "$type" '[.status.resourceRecords[]? | select(.type == $t) | .rrdata] | join(",")')
  [ -n "$rr" ] || continue
  g dns record-sets describe "$DOMAIN." --type "$type" --zone "$ZONE" >/dev/null 2>&1 ||
    g dns record-sets create "$DOMAIN." --type "$type" --ttl 300 --rrdatas "$rr" --zone "$ZONE"
done

for host in $HUB_ALIASES; do
  log "alias $host"
  g beta run domain-mappings describe --domain "$host" --region "$REGION" >/dev/null 2>&1 ||
    g beta run domain-mappings create --service "$NAME" --domain "$host" --region "$REGION"
  g dns record-sets describe "$host." --type CNAME --zone "$ZONE" >/dev/null 2>&1 ||
    g dns record-sets create "$host." --type CNAME --ttl 300 --rrdatas ghs.googlehosted.com. --zone "$ZONE"
done

log "github: $PLATFORM_REPO deploy variables"
if gh auth status >/dev/null 2>&1; then
  set_var() { gh variable set "$1" -R "$PLATFORM_REPO" --body "$2" >/dev/null; }
  set_var GCP_PROJECT "$PROJECT"
  set_var GCP_REGION "$REGION"
  set_var GCP_WIF_PROVIDER "projects/$NUMBER/locations/global/workloadIdentityPools/$NAME/providers/platform"
  set_var GCP_DEPLOYER "$(sa deployer)"
  set_var IMAGE "$REGION-docker.pkg.dev/$PROJECT/$NAME"
  set_var SERVICE "$NAME"
  set_var JOB "$NAME-runner"
  set_var PROVISION_JOB "$NAME-provision"
else
  echo "!! gh not signed in: set $PLATFORM_REPO's deploy variables by hand"
fi
log "done; set DEPLOY_ON=push in $PLATFORM_REPO's app.env to deploy the hub"
