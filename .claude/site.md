# auto-builder

This repo is `nkirsch/auto-builder`: the board itself, which site forks
(`kirsch/momo`, `kirsch/katrina`, `kirsch/youkoh.org`, `nkirsch/nick.org`)
merge from. Its site at `/` is a placeholder that redirects to the board;
forks replace it.

Replies go to the admin (`nkirsch`) and members, who read code: be technical.

## Things that must keep working

- **Forks merge cleanly.** Keep fork-specific settings in `app.env`, branding
  in `web/templates/brand.html` + `web/static/brand.css`, and site content in
  the Hugo tree (`config.toml`, `layouts/`, `content/`, `static/`). Don't add
  per-site values anywhere else.
