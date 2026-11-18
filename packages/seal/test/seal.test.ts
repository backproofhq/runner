import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { describe, it } from "node:test";

import { armor, Decrypter } from "age-encryption";

import {
  type BundleSecrets,
  createKeyPair,
  createNameSalt,
  keyFingerprint,
  sealBundle,
  SealError,
} from "../src/index.ts";

const secrets = (): BundleSecrets => ({
  s3_access_key: `access-${crypto.randomUUID()}`,
  s3_secret_key: `secret-${crypto.randomUUID()}`,
  name_salt: createNameSalt(),
});

async function open(sealed: string, identity: string): Promise<Record<string, unknown>> {
  const decrypter = new Decrypter();
  decrypter.addIdentity(identity);
  const plain = await decrypter.decrypt(armor.decode(sealed), "text");
  return JSON.parse(plain) as Record<string, unknown>;
}

const refusedWith = (code: string, messagePart?: RegExp) => (error: unknown) => {
  assert.ok(error instanceof SealError);
  assert.equal(error.code, code);
  if (messagePart) assert.match(error.message, messagePart);
  return true;
};

describe("sealBundle", () => {
  it("returns an armored age file that the runner's key opens", async () => {
    const { identity, recipient } = await createKeyPair();
    const input = secrets();

    const sealed = await sealBundle(input, [recipient]);

    assert.ok(sealed.startsWith("-----BEGIN AGE ENCRYPTED FILE-----\n"));
    assert.deepEqual(await open(sealed, identity), { version: 1, ...input });
  });

  it("holds no plain value", async () => {
    const { recipient } = await createKeyPair();
    const input = secrets();

    const sealed = await sealBundle(input, [recipient]);

    for (const value of Object.values(input)) assert.ok(!sealed.includes(value));
  });

  it("opens with any of the runners' keys, and not with another key", async () => {
    const [first, second, stranger] = await Promise.all([
      createKeyPair(),
      createKeyPair(),
      createKeyPair(),
    ]);
    const input = secrets();

    const sealed = await sealBundle(input, [first.recipient, second.recipient]);

    assert.equal((await open(sealed, first.identity)).s3_secret_key, input.s3_secret_key);
    assert.equal((await open(sealed, second.identity)).s3_secret_key, input.s3_secret_key);
    await assert.rejects(open(sealed, stranger.identity));
  });

  it("includes the optional keys when they are set, and leaves out empty ones", async () => {
    const { identity, recipient } = await createKeyPair();
    const input = {
      ...secrets(),
      age_identity: "test-age-identity",
      gpg_key: "",
      gpg_passphrase: undefined,
    };

    const opened = await open(await sealBundle(input, [recipient]), identity);

    assert.equal(opened.age_identity, "test-age-identity");
    assert.ok(!("gpg_key" in opened));
    assert.ok(!("gpg_passphrase" in opened));
  });

  it("drops fields the contract doesn't define and can't be given another version", async () => {
    const { identity, recipient } = await createKeyPair();
    const input = { ...secrets(), version: 9, extra: "value" } as BundleSecrets;

    const opened = await open(await sealBundle(input, [recipient]), identity);

    assert.equal(opened.version, 1);
    assert.ok(!("extra" in opened));
  });

  for (const field of ["s3_access_key", "s3_secret_key", "name_salt"] as const) {
    it(`requires ${field}`, async () => {
      const { recipient } = await createKeyPair();
      await assert.rejects(
        sealBundle({ ...secrets(), [field]: "" }, [recipient]),
        refusedWith("invalid_secrets", new RegExp(field)),
      );
    });
  }

  it("requires a name_salt of at least 32 characters", async () => {
    const { identity, recipient } = await createKeyPair();

    await assert.rejects(
      sealBundle({ ...secrets(), name_salt: "x".repeat(31) }, [recipient]),
      refusedWith("invalid_secrets", /32 characters/),
    );
    const salt = "🧂".repeat(32);
    const opened = await open(
      await sealBundle({ ...secrets(), name_salt: salt }, [recipient]),
      identity,
    );
    assert.equal(opened.name_salt, salt);
  });

  it("rejects an empty recipient list", async () => {
    await assert.rejects(sealBundle(secrets(), []), refusedWith("invalid_recipients"));
  });

  it("rejects an invalid key", async () => {
    const { recipient } = await createKeyPair();
    const lastCharacter = recipient.endsWith("q") ? "p" : "q";
    const badChecksum = `${recipient.slice(0, -1)}${lastCharacter}`;

    for (const key of ["", "age1test", "ssh-ed25519 AAAA", recipient.toUpperCase(), badChecksum]) {
      await assert.rejects(
        sealBundle(secrets(), [recipient, key]),
        refusedWith("invalid_recipients", /public key 2/),
      );
    }
  });

  it("rejects a duplicate key", async () => {
    const { recipient } = await createKeyPair();
    await assert.rejects(
      sealBundle(secrets(), [recipient, recipient]),
      refusedWith("invalid_recipients", /twice/),
    );
  });

  it("rejects keys too large for the contract", async () => {
    const { recipient } = await createKeyPair();
    await assert.rejects(
      sealBundle({ ...secrets(), gpg_key: "x".repeat(1_048_576) }, [recipient]),
      refusedWith("bundle_too_large"),
    );
  });

  it("never puts a secret in an error message", async () => {
    const { recipient } = await createKeyPair();
    const secret = "test-secret-value-that-must-stay-out";
    const attempts = [
      sealBundle({ ...secrets(), s3_secret_key: secret, name_salt: "short" }, [recipient]),
      sealBundle({ ...secrets(), s3_access_key: secret }, ["not-a-key"]),
      sealBundle({ ...secrets(), name_salt: secret.slice(0, 31) }, [recipient]),
      sealBundle({ ...secrets(), gpg_key: secret.repeat(40_000) }, [recipient]),
    ];

    for (const attempt of attempts) {
      await assert.rejects(attempt, (error: Error) => {
        assert.ok(!`${error.message}${error.stack}`.includes(secret.slice(0, 20)));
        assert.equal(error.cause, undefined);
        return true;
      });
    }
  });
});

describe("createNameSalt", () => {
  it("creates a different salt of at least 32 URL-safe characters each time", () => {
    const salt = createNameSalt();

    assert.match(salt, /^[A-Za-z0-9_-]{43}$/);
    assert.notEqual(createNameSalt(), salt);
  });
});

describe("createKeyPair", () => {
  it("creates a matching X25519 key pair", async () => {
    const { identity, recipient } = await createKeyPair();

    assert.match(identity, /^AGE-SECRET-KEY-1[QPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L]{58}$/);
    assert.match(recipient, /^age1[qpzry9x8gf2tvdw0s3jn54khce6mua7l]{58}$/);
    assert.notEqual((await createKeyPair()).identity, identity);
  });
});

describe("keyFingerprint", () => {
  it("matches the web app and the runner", async () => {
    assert.equal(
      await keyFingerprint("age1test"),
      "SHA256:4m4FJEkQ1+4bsddQHkzIqik856xUVFF2r27Q/fZPLno",
    );
  });
});

describe("the built package", () => {
  it("uses no Node.js modules, so it runs in the browser", async () => {
    const built = await readFile(new URL("../dist/index.js", import.meta.url), "utf8");

    assert.doesNotMatch(built, /from\s+["']node:|require\(/);
  });
});
