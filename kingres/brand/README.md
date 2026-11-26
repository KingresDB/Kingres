<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# KingresDB brand assets

The logo files of KingresDB, as supplied by the project owner. They are the
source of truth: other parts of the repository (the web panel, the server's
placeholder page, the README files) copy from here or redraw the same paths,
and do not change the shapes.

The logo is a crown above a shield that carries the letters "DB". The
wordmark reads "KINGRES DB", with "KINGRES" in bold and "DB" in a light
weight.

## Files

Every variant exists as SVG (`svg/`) and PNG (`png/`).

| Variant | File name | What it is | Use it for |
|---|---|---|---|
| Mark | `kingres-db-mark-*` | The crown and shield alone, square | Small spaces: navigation bars, avatars, loading states |
| Horizontal | `kingres-db-horizontal-*` | Mark with the wordmark to its right | Page headers, README files, documents |
| Stacked | `kingres-db-stacked-*` | Mark with the wordmark below it, square | Sign-in and first-run screens, covers |
| App icon | `kingres-db-app-icon` | Mark on a dark rounded square | Application and home-screen icons |
| Favicon | `png/favicon-32.png`, `favicon-180.png`, `favicon-512.png` | App icon at browser sizes | Browser tab, Apple touch icon, web app manifest |

Each of mark, horizontal and stacked comes in five colorings:

| Suffix | Background | Crown and "KINGRES" | Shield | "DB" |
|---|---|---|---|---|
| `color-dark` | Dark, filled | Cream | Gold gradient | Light gold |
| `color-light` | White, filled | Ink | Gold gradient | Deep gold |
| `color-transparent` | None | Ink (gold in the mark) | Gold gradient | Deep gold |
| `mono-black` | None | Black | Black | Black |
| `mono-white` | None | White | White | White |

## Colors

| Name | Value | Where it appears |
|---|---|---|
| Light gold | `#F6D365` | Top of the shield gradient; "DB" on dark |
| Deep gold | `#C8962E` | Bottom of the shield gradient; "DB" on light |
| Ink | `#14171F` | Crown and "KINGRES" on light backgrounds |
| Night | `#0B0D12` | The dark background |
| Cream | `#F1EADB` | Crown and "KINGRES" on dark backgrounds |

The shield gradient runs top to bottom from light gold to deep gold.

## Rules

- Do not redraw, stretch, rotate or recolor the logo beyond the colorings
  listed above.
- On a dark surface use the cream-and-gold coloring; on a light surface the
  ink-and-gold coloring; where color is not available use a mono variant.
- Keep clear space around the logo of at least the height of the crown.
- Do not place the color variants on a background that leaves the gold with
  too little contrast; use a mono variant there.
- Deep gold on white does not have enough contrast for body text or small
  controls. In user interfaces the gold is a brand accent, not a text color.
