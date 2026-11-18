# @backproof/seal

Seals storage and decryption keys for Backproof runners. The dashboard runs this code in the browser, so plain keys never reach Backproof. Only runners whose private key the bundle was sealed for can open it.

The format is `SealedBundleContents` in [`api/v1/openapi.yaml`](../../api/v1/openapi.yaml). The runner opens a bundle with `OpenSealedBundle` in `internal/crypto`.

```ts
import { createNameSalt, keyFingerprint, sealBundle } from "@backproof/seal";

const sealedBundle = await sealBundle(
  {
    s3_access_key: accessKey,
    s3_secret_key: secretKey,
    name_salt: createNameSalt(),
  },
  [runnerPublicKey],
);

const fingerprint = await keyFingerprint(runnerPublicKey);
```

| Export | What it does |
| --- | --- |
| `sealBundle(secrets, recipients)` | Returns an ASCII-armored age file for the given `age1...` public keys |
| `createNameSalt()` | Returns a random salt for hashing table names |
| `createKeyPair()` | Returns a new age X25519 key pair |
| `keyFingerprint(publicKey)` | Returns the fingerprint users compare with the runner's |

Invalid input throws a `SealError`, whose message never contains a secret value.

## Installing

The package is installed from Git and has no install step, so the built `dist/` folder is committed. Run `npm run build` after changing `src/`. CI fails when `dist/` is out of date.

```sh
pnpm add "github:backproofhq/runner#<commit>&path:/packages/seal"
```
