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
export type SealErrorCode = "invalid_secrets" | "invalid_recipients" | "bundle_too_large" | "seal_failed";
/** Messages name a field or a position in the list, and never contain a value. */
export declare class SealError extends Error {
    readonly name = "SealError";
    readonly code: SealErrorCode;
    constructor(code: SealErrorCode, message: string);
}
/**
 * Seals the keys for the given runners and returns an ASCII-armored age file. Any one of
 * the runners' private keys opens it.
 */
export declare function sealBundle(secrets: BundleSecrets, recipients: readonly string[]): Promise<string>;
/** A random key for hashing table names, as URL-safe text. */
export declare function createNameSalt(): string;
export interface KeyPair {
    /** The `AGE-SECRET-KEY-1...` private key. */
    identity: string;
    /** The `age1...` public key. */
    recipient: string;
}
/** A new age X25519 key pair. */
export declare function createKeyPair(): Promise<KeyPair>;
/**
 * What users compare between the dashboard, the install script and `backproof doctor`:
 * `SHA256:` and the unpadded base64 of the SHA-256 of the `age1...` string.
 */
export declare function keyFingerprint(publicKey: string): Promise<string>;
