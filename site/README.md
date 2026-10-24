# Mailinois site

The marketing page and documentation for Mailinois, built with [Astro](https://astro.build) and
[Starlight](https://starlight.astro.build).

## Run it locally

Needs Node 22.12 or newer.

```sh
npm install
npm run dev       # http://localhost:4321/mailinois/
npm run build     # static site in dist/
npm run preview   # serve dist/
```

The landing page is `src/pages/index.astro`. The docs are Markdown in `src/content/docs/docs/`, and the
sidebar order is in `astro.config.mjs`.

## Interactive demo

`/demo/` is a standalone browser demo, linked from the homepage beside the example report. `src/pages/demo.astro`
provides the shell and imports the app's real stylesheet from `../internal/web/static/app.css`. The model and
views in `src/demo/` run entirely in JavaScript: no app server, mail relay or API. All traffic, addresses and
check results are made up. Its assets are bundled and fingerprinted by Astro.

The simulation has three scenarios, site/server/period filters, message search and details, a queue, provider
summaries, expandable domain checks, server checks and alerts. Pause holds the data; reset or reload restores it.
Only the theme preference is persisted. `npm test` checks the model, search, routing and rendering (Node 22.12+).

## Deploying

`.github/workflows/pages.yml` builds it and publishes it to GitHub Pages on every push to `master` that touches
`site/`, at https://samjuk.github.io/mailinois/. Links in Markdown are relative (`../../docs/api/`) because
Astro doesn't add the base path to them. With a custom domain, set `site` to it and `base` to `'/'` at the top
of `astro.config.mjs`, and add the domain in the repository's Pages settings.
