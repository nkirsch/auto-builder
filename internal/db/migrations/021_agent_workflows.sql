-- whether a run's agent may push changes under .github/workflows/
ALTER TABLE settings ADD COLUMN agent_workflows BOOLEAN NOT NULL DEFAULT true;
