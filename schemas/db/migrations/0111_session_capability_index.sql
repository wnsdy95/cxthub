CREATE INDEX sessions_capability_hash_idx ON sessions
((CASE WHEN left(token,4)='tkh_' THEN token
 WHEN token ~ '^sess_([0-9a-f]{32}|cli_[0-9a-f]{32})$'
 THEN 'tkh_' || encode(sha256(decode(token,'escape')),'hex') END));
