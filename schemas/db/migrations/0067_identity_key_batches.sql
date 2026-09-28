-- Operator receipts contain counts, key identifiers and cursor metadata only.
-- Ciphertext/plaintext never enters this audit log. Each batch commits with its
-- replacements under the same identity transaction and is safe to retry.
CREATE TABLE identity_key_batches (
 operation TEXT NOT NULL,
 after_cursor TEXT NOT NULL,
 record JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(operation,after_cursor)
);
