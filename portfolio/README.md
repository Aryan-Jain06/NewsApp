# aryan-jain06.github.io

Personal portfolio — [aryan-jain06.github.io](https://aryan-jain06.github.io)

Plain HTML, CSS and vanilla JS. No framework, no build step, no dependencies:
what is in this repo is exactly what the browser gets.

## Layout

```
index.html        hero, focus areas, featured work
experience.html   role + education timeline, competitive record
projects.html     project cards
skills.html       grouped toolkit, working habits
contact.html      email, links, resume
404.html          fallback page
styles.css        the whole design system (tokens at the top)
main.js           mobile nav, scroll reveal, copy-to-clipboard
assets/           favicon, OpenGraph image, resume PDF
```

## Editing

- **Colour, spacing, type** — every value lives in the `:root` block at the top
  of `styles.css`. Changing `--accent` re-themes the entire site.
- **Resume** — replace `assets/Aryan_Jain_Resume.pdf`, keeping the filename, and
  every link picks it up.
- **A new project card** — copy an `<article class="card proj">` block in
  `projects.html` and edit the text. Card heights stay aligned automatically.

## Running locally

```bash
python3 -m http.server 8000
# then open http://localhost:8000
```

## Deployment

GitHub Pages serves the default branch from the repository root. Because the
repo is named `aryan-jain06.github.io`, the site is published at the bare
domain — `https://aryan-jain06.github.io` loads `index.html` directly.
