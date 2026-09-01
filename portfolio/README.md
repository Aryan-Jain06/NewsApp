# aryan-jain06.github.io

Personal portfolio — [aryan-jain06.github.io](https://aryan-jain06.github.io)

Plain HTML, CSS and vanilla JS. No framework, no build step, no dependencies,
and no third-party requests at runtime: what is in this repo is exactly what
the browser gets.

## Layout

```
index.html          hero, background, two featured projects
experience.html     role + education timeline, competitive programming
projects.html       project cards
skills.html         grouped skills
contact.html        email, links, resume
404.html            fallback page
styles.css          the whole design system (tokens at the top)
main.js             mobile nav, scroll reveal, copy-to-clipboard
manifest.webmanifest  home-screen name and icons
assets/             icons, OpenGraph image, resume PDF
assets/fonts/       self-hosted woff2 + @font-face rules
```

## Editing

- **Colour, spacing, type** — every value lives in the `:root` block at the top
  of `styles.css`. Changing `--accent` re-themes the whole site. The
  `@media (prefers-color-scheme: dark)` block right below it holds the dark
  palette; both are token-only, nothing downstream hardcodes a colour.
- **Resume** — replace `assets/Aryan_Jain_Resume.pdf`, keeping the filename,
  and every link picks it up.
- **A new project card** — copy an `<article class="card proj">` block in
  `projects.html` and edit the text. Card heights stay aligned automatically.
- **Changing domain** — the canonical and OpenGraph tags carry an absolute URL,
  which is the one thing tied to a hostname:

  ```bash
  grep -rl 'aryan-jain06.github.io' . \
    | xargs sed -i 's|https://aryan-jain06.github.io|https://your-domain.com|g'
  ```

## Running locally

```bash
python3 -m http.server 8000
# then open http://localhost:8000
```

It also works opened straight off disk (`file://`), since every path is
relative and the fonts ship with the site.

## Deploying

The site is static files in the repository root, so any host works.

- **GitHub Pages** — a `<user>.github.io` repository serves its root
  automatically; `https://aryan-jain06.github.io` loads `index.html` with no
  path. `.github/workflows/pages.yml` is there if you would rather deploy
  through Actions.
- **Netlify / Cloudflare Pages** — `netlify.toml`: publish `.`, no build
  command.
- **Vercel** — `vercel.json`: no build, clean URLs.
- **Anything else** (S3, nginx, Firebase) — upload the directory as-is.

## Notes

- Fonts (Space Grotesk, Inter, JetBrains Mono — all OFL 1.1) are self-hosted
  as variable woff2 in `assets/fonts/`, split latin / latin-ext by
  `unicode-range`. Around 100 KB actually transfers for English text.
- Icons ship as SVG plus PNG at 32, 180, 192 and 512 px, because iOS ignores
  SVG for the home-screen icon.
- Respects `prefers-reduced-motion`, and the scroll animations are opt-in
  behind a `.js` class so the content is fully visible with JavaScript off.
