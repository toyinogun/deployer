# 0026. Source carried inline on deploy_app

**Date**: 2026-08-16
**Status**: Accepted

## Summary

`deploy_app` took only an `upload_id`, and the only way to mint one was `POST /v1/uploads`, a plain HTTP request outside the tool set. A connector client with no shell, which is what a person signing in from claude.ai actually has, can call every tool on the platform except the one thing it needs first, so the deploy path was unreachable for the caller it was published for in spec 0022. This adds an optional `files` argument: a map of path relative to the app's root to that file's content. The platform packs it into the same gzipped tarball the endpoint would have received and hands it to the same upload service, so the inline path is a second way to reach the upload rather than a way around it.

## Requirements

**User story**: As a person driving the platform from a chat client with no shell, I want to hand `deploy_app` the source of the app the agent just wrote, so that deploying is one tool call rather than a curl I have to run myself and paste an id back from.

**Acceptance criteria**:

- **AC-1**: `deploy_app` takes `files`, a map of path to file content, alongside `name`. Text only: there is no encoding argument and no way to carry a binary.
- **AC-2**: Exactly one of `upload_id` and `files` is given. Both, neither, and an empty `files` map are all refused `upload_invalid`, create no app row and start no deployment.
- **AC-3**: A path that is empty, absolute, holds a NUL, or climbs out of the tree with `..` is refused `upload_invalid` before any of the set is packed. The refusal happens here as well as at extraction, so a caller learns about it instead of spending a build pod on it.
- **AC-4**: The packed archive goes through `uploads.Service.Accept`, so it costs the account the same unclaimed slot, is refused `upload_too_large` over the same `DEPLOYER_MAX_UPLOAD_BYTES` ceiling and `upload_limit_reached` at the same unclaimed cap, expires in the same window, and is swept by the same sweep.
- **AC-5**: Entries are regular files at mode 0644 with no directory entries, packed in sorted order, so the same set of files packs to the same bytes every time. The extractor in `internal/source` creates each file's parent itself.
- **AC-6**: Nothing else about the deploy changes. The build path is still chosen by reading the archive's tar headers, so a `Dockerfile` key in `files` selects BuildKit exactly as an uploaded one does, and the call still returns `queued` without waiting.
- **AC-7**: `deploy_app`'s description states both ways of giving the source and that they are exclusive. The upload endpoint and the ceiling are still derived from configuration rather than written as literals.

## Decision

**Chosen option**: An optional `files` map on `deploy_app`, packed by `uploads.Pack` and accepted by the existing service.

Rejected: a `deploy_from_git` tool, which needs the source pushed somewhere first and egress from the build path to a forge; and a base64 tarball argument, which is the smallest server change but asks a model to emit hundreds of kilobytes of base64 accurately.

The bound on what this can cost the platform is entirely `Accept`'s, deliberately. `Pack` composes in memory and bounds nothing, because the transport bounds the request body long before the ceiling does and a second number here would be a second place for the ceiling to drift.
