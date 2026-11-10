# Vendored Swagger UI assets

These files are copied unmodified from the public npm package
`swagger-ui-dist@5.34.1` (Apache-2.0, upstream https://github.com/swagger-api/swagger-ui),
obtained with `npm pack swagger-ui-dist` and extracted with `tar`. They are
embedded into `vmapi` so the browser never contacts a CDN.

| File | SHA-256 |
| --- | --- |
| swagger-ui-bundle.js | 36eeb4d5640b9c036b1d0f854499af1cceebbdb260b790a03a18be5e8488fbf5 |
| swagger-ui-bundle.js.LICENSE.txt | 37eb6e7e59aed57b2c1acf076574b818ea07b74265717e0bd67b59e0baf9abbb |
| swagger-ui.css | c3cfe8248c4b0b89673600fb829924eafbac6de4bc4b2b06f5f5243483074d0a |
| favicon-32x32.png | 3ed612f41e050ca5e7000cad6f1cbe7e7da39f65fca99c02e99e6591056e5837 |
| LICENSE | cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30 |
| NOTICE | (upstream, "swagger-ui / Copyright 2020-2021 SmartBear Software Inc.") |

Tarball SHA-256 (swagger-ui-dist-5.34.1.tgz):
`8e7286de0230e431fc8a5a75490d28117af3d0c306413b6f9e17f7bb55045fb6`.

`swagger-initializer.js` is the only file written for this project; it is not
upstream code. A test verifies the checksums above against the embedded files.
To upgrade, repeat the fetch, replace the files and update this table.
