# Console mock

A clickable mock of the web console on fixed data (`src/mock/`), kept as the
reference for pages and interactions the real console in `web/` does not have
yet: the command palette, networks, security groups, addresses, public
endpoints, disks, images, secrets, members and service tokens, and the size
cards at creation.

It talks to no API and is not part of a release. The settled visual design is
in `docs/decisions/console-design.md`; where the two differ, that document and
`web/` win.

```sh
pnpm install
pnpm --dir web-mock dev
```
