-- the provisioner's progress: [{name, state (pending/running/done/failed), error}] in order
ALTER TABLE tenants ADD COLUMN steps JSONB NOT NULL DEFAULT '[]';
