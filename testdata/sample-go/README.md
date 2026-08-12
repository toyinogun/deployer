# sample-go

The app the first end to end deploy deploys. Kept here rather than in the image,
so CI can tar it without shipping it.

Deploy it from an agent session:

```bash
curl -sS -X POST "$DEPLOYER_PUBLIC_URL/v1/uploads" \
  -H "Authorization: Bearer $DEPLOYER_TOKEN" \
  --data-binary @- < <(cd testdata/sample-go && tar czf - .)
```

Then call `deploy_app` with the upload id it returns.
