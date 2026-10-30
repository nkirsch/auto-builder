-- A member's GitHub account, verified by them signing in to GitHub to connect it.
ALTER TABLE members ADD COLUMN github_login text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX members_github_login ON members (lower(github_login)) WHERE github_login <> '';
