-- auto-approve is on by default: admins file their own asks
ALTER TABLE members ALTER COLUMN auto_approve SET DEFAULT true;
UPDATE members SET auto_approve = true WHERE role = 'admin';
