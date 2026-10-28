-- an admin's connected Gmail for sending invites: address and sealed refresh token
ALTER TABLE members ADD COLUMN gmail_address TEXT NOT NULL DEFAULT '',
    ADD COLUMN gmail_token TEXT NOT NULL DEFAULT '';
