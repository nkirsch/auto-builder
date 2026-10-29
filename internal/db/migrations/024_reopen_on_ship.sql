-- an admin replied while the item was merged: reopen it once it ships
ALTER TABLE items ADD COLUMN reopen_on_ship BOOLEAN NOT NULL DEFAULT false;
