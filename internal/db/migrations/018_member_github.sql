-- a member with an email may also sign in with GitHub when their login is their own GitHub account
ALTER TABLE members ADD COLUMN github BOOLEAN NOT NULL DEFAULT false;
