// Called by test/seal_interop_test.go at the repo root: reads a request as JSON on stdin and
// writes the answer as JSON on stdout. Keys go over stdin so they never appear in a process list.
import { createKeyPair, createNameSalt, keyFingerprint, sealBundle } from "../dist/index.js";

let input = "";
for await (const chunk of process.stdin) input += chunk;
const { secrets, recipients } = JSON.parse(input);

const pair = await createKeyPair();
const salt = createNameSalt();
const sealed = await sealBundle({ ...secrets, name_salt: secrets.name_salt ?? salt }, recipients);
const ownSealed = await sealBundle({ ...secrets, name_salt: salt }, [pair.recipient]);

process.stdout.write(
  JSON.stringify({
    sealed,
    salt,
    pair,
    ownSealed,
    fingerprint: await keyFingerprint(pair.recipient),
  }),
);
