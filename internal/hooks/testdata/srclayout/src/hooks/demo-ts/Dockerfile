# The tree-mirror COPY convention for src-layout hooks: the build context
# is <repo>/src (the runner passes this Dockerfile with -f), and the image
# mirrors the repo tree under /app so the SAME relative import
# (../../sdk/...) resolves in-repo (editor, tests) and in-image (runtime).
# Node 22's native type-stripping runs the .ts directly — the fixture
# needs no registry access at build time.
FROM node:22-alpine
COPY sdk/ /app/sdk/
COPY hooks/demo-ts/ /app/hooks/demo-ts/
WORKDIR /app/hooks/demo-ts
CMD ["node", "--experimental-strip-types", "main.ts"]
