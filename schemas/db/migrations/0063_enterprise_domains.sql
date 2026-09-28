-- Domain ownership is independent of membership and login identity.
CREATE TABLE IF NOT EXISTS enterprise_domains (
  enterprise_id TEXT NOT NULL REFERENCES enterprises(id),
  domain TEXT NOT NULL,
  record JSONB NOT NULL,
  verified_until TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (enterprise_id, domain)
);
CREATE INDEX IF NOT EXISTS enterprise_domains_claim ON enterprise_domains(domain, verified_until);
