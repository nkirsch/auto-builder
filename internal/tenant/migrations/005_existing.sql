-- an existing repo that deploys itself: no provisioning, shipped from its app's /health
ALTER TABLE tenants ADD COLUMN existing BOOLEAN NOT NULL DEFAULT false;
