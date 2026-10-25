-- admins who auto-approve their own asks and items the agent splits from their comments
ALTER TABLE members ADD COLUMN auto_approve BOOLEAN NOT NULL DEFAULT false;
