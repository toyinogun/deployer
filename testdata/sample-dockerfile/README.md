# sample-dockerfile

The app the Dockerfile build path deploys. Its whole job is to prove that a
project shipping a `Dockerfile` at its root is built as written, while
[sample-go](../sample-go) with no Dockerfile still goes through Buildpacks.

Deploy it the same way as any other app:

```bash
curl -sS -X POST "$DEPLOYER_PUBLIC_URL/v1/uploads" \
  -H "Authorization: Bearer $DEPLOYER_TOKEN" \
  --data-binary @- < <(cd testdata/sample-dockerfile && tar czf - .)
```

Then call `deploy_app` with the upload id it returns. `deployment_status` reports
`build_path: dockerfile` for it, and `build_path: buildpacks` for sample-go.

Two refusals are proved by editing a copy of the Dockerfile before uploading:

- drop the `USER 1000` line, and the deployment fails with `image_runs_as_root`
  before any app object is composed
- add a `RUN false` line, and it fails with `build_failed`, with the build output
  staying in the build pod's own logs
