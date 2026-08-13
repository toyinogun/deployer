# envprint

The app that proves spec 0010's configuration actually reaches a running
container. It logs every environment variable it was given, so `get_logs` shows
`PORT`, `APP_URL`, and each configured key.

Set `SENTENCE_KEY` to the name of another key and it also logs one ordinary
sentence carrying that key's value, which is how the redaction rule is checked
both ways: a secret value eight characters or longer is blanked out of the
sentence, and a shorter one is left alone.

```bash
curl -sS -X POST "$DEPLOYER_PUBLIC_URL/v1/uploads" \
  -H "Authorization: Bearer $DEPLOYER_TOKEN" \
  --data-binary @- < <(cd testdata/envprint && tar czf - .)
```

Then call `deploy_app` with the upload id it returns.
