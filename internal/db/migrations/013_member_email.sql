-- Google sign-in finds the member by verified email; '' means GitHub sign-in only
ALTER TABLE members ADD COLUMN email TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX members_email ON members (lower(email)) WHERE email <> '';
