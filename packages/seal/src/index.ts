import { armor, Encrypter, generateX25519Identity, identityToRecipient } from "age-encryption";

/** `SealedBundleContents` in api/v1/openapi.yaml, without the version this package sets. */
export interface BundleSecrets {
  s3_access_key: string;
  s3_secret_key: string;
  /** At least 32 characters. Use {@link createNameSalt}. */
  name_salt: string;
  age_identity?: string | undefined;
  gpg_key?: string | undefined;
  gpg_passphrase?: string | undefined;
}

export type SealErrorCode =
  | "invalid_secrets"
  | "invalid_recipients"
  | "bundle_too_large"
  | "seal_failed";

/** Messages name a field or a position in the list, and never contain a value. */
export class SealError extends Error {
  override readonly name = "SealError";
  readonly code: SealErrorCode;

  constructor(code: SealErrorCode, message: string) {
    super(message);
    this.code = code;
  }
}

const bundleVersion = 1;
const minNameSaltLength = 32;
const nameSaltBytes = 32;
/** `Job.sealed_bundle` in api/v1/openapi.yaml. */
const maxBundleLength = 1_048_576;
const recipientFormat = /^age1[qpzry9x8gf2tvdw0s3jn54khce6mua7l]{58}$/;

const required = ["s3_access_key", "s3_secret_key", "name_salt"] as const;
const optional = ["age_identity", "gpg_key", "gpg_passphrase"] as const;

function bundleContents(secrets: BundleSecrets): string {
  const contents: Record<string, string | number> = { version: bundleVersion };
  for (const field of required) {
    const value: unknown = secrets?.[field];
    if (typeof value !== "string" || value === "") {
      throw new SealError("invalid_secrets", `${field} is required.`);
    }
    contents[field] = value;
  }
  // JSON Schema's minLength counts characters, not UTF-16 units.
  if ([...secrets.name_salt].length < minNameSaltLength) {
    throw new SealError(
      "invalid_secrets",
      `name_salt must be at least ${minNameSaltLength} characters.`,
    );
  }
  for (const field of optional) {
    const value: unknown = secrets[field];
    if (value === undefined || value === "") continue;
    if (typeof value !== "string") {
      throw new SealError("invalid_secrets", `${field} must be text.`);
    }
    contents[field] = value;
  }
  return JSON.stringify(contents);
}

function encrypterFor(recipients: readonly string[]): Encrypter {
  if (!Array.isArray(recipients) || recipients.length === 0) {
    throw new SealError("invalid_recipients", "Seal the bundle for at least one runner.");
  }
  const encrypter = new Encrypter();
  const seen = new Set<string>();
  recipients.forEach((recipient: unknown, index) => {
    const position = index + 1;
    const invalid = new SealError(
      "invalid_recipients",
      `Runner public key ${position} isn't a valid age key.`,
    );
    if (typeof recipient !== "string" || !recipientFormat.test(recipient)) throw invalid;
    if (seen.has(recipient)) {
      throw new SealError(
        "invalid_recipients",
        `Runner public key ${position} is in the list twice.`,
      );
    }
    seen.add(recipient);
    try {
      encrypter.addRecipient(recipient);
    } catch {
      throw invalid;
    }
  });
  return encrypter;
}

/**
 * Seals the keys for the given runners and returns an ASCII-armored age file. Any one of
 * the runners' private keys opens it.
 */
export async function sealBundle(
  secrets: BundleSecrets,
  recipients: readonly string[],
): Promise<string> {
  const encrypter = encrypterFor(recipients);
  const contents = bundleContents(secrets);

  let sealed: string;
  try {
    sealed = armor.encode(await encrypter.encrypt(contents));
  } catch {
    // A library error could describe its input, so it isn't passed on.
    throw new SealError("seal_failed", "The keys couldn't be sealed for these runners.");
  }
  if (sealed.length > maxBundleLength) {
    throw new SealError("bundle_too_large", "These keys are too large to seal.");
  }
  return sealed;
}

function toBase64(bytes: Uint8Array): string {
  return btoa(String.fromCharCode(...bytes));
}

/** A random key for hashing table names, as URL-safe text. */
export function createNameSalt(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(nameSaltBytes));
  return toBase64(bytes).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

export interface KeyPair {
  /** The `AGE-SECRET-KEY-1...` private key. */
  identity: string;
  /** The `age1...` public key. */
  recipient: string;
}

/** A new age X25519 key pair. */
export async function createKeyPair(): Promise<KeyPair> {
  const identity = await generateX25519Identity();
  return { identity, recipient: await identityToRecipient(identity) };
}

/**
 * What users compare between the dashboard, the install script and `backproof doctor`:
 * `SHA256:` and the unpadded base64 of the SHA-256 of the `age1...` string.
 */
export async function keyFingerprint(publicKey: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(publicKey));
  return `SHA256:${toBase64(new Uint8Array(digest)).replace(/=+$/, "")}`;
}
