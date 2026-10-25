-- Bot-filed items used -infinity, which pgx can't scan into time.Time.
UPDATE items SET human_activity_at = 'epoch' WHERE human_activity_at = '-infinity';
