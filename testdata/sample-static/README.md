# sample-static

A plain HTML page with no build step, which is what a non technical person gets
when they ask a coding agent for a simple site. It exists because that case
fails on the Buildpacks path and nothing in the repo said so.

Deploy it the same way as any other app:

```bash
curl -sS -X POST "$DEPLOYER_PUBLIC_URL/v1/uploads" \
  -H "Authorization: Bearer $DEPLOYER_TOKEN" \
  --data-binary @- < <(cd testdata/sample-static && tar czf - .)
```

Then call `deploy_app` with the upload id it returns. It reports
`build_path: dockerfile` and reaches healthy in about ninety seconds.

## What this sample records

Measured against the real cluster on 2026-08-14, with the builder pinned at the
time:

- **Uploading `index.html` alone, with no Dockerfile, fails.** The build ends
  `build_failed` after about nine seconds and the build pod's log ends with
  `ERROR: No buildpack groups passed detection`. There is no static buildpack in
  the group that a page on its own can satisfy.
- **`nginx.conf` at the root does pass detection**, because
  `paketo-buildpacks/nginx` is in the builder. It is still the wrong answer to
  reach for: getting a working configuration took six deploys, each failing on
  something invisible from outside the container, a duplicate `pid` directive, a
  missing `mime.types`, a `uwsgi_temp_path` the module was not built with, a
  permission denied on `fastcgi_temp`, and a document root that is
  `/workspace/source/public` rather than `/workspace/public`. A person who
  cannot read an nginx error is stuck, and an agent guesses at it slowly.
- **The Dockerfile here worked first time**, which is why it is the one the
  `deploy_app` description points at.

One thing worth knowing that this sample surfaced: an app serving `404` still
reaches `healthy`, because readiness means the container accepted a connection
on `PORT`, not that it answers correctly.
