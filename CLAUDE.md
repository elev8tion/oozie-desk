# Oozie Desk — this clone

This directory is a web-oriented fork of oozie. It is not the Mac app.

- Edit only this tree (`Oozie-Desk`).
- Do not modify `/Users/kc/oozie`, `/Applications/oozie.app`, or
  `~/Library/Application Support/oozie`.
- This fork stores its database at
  `~/Library/Application Support/Oozie-Desk/app.db` (or the older
  `oozie-web` folder if that already exists).
- `make run` serves the factory at http://127.0.0.1:8090.
- Publish compiles a Go module in the project and Start serves that binary
  on a free localhost port. There is no `.app` bundle and no `/Applications`
  install.

The original Mac factory stays where it is. This clone does not replace it.
