-- Private alternate keys stay out of the public connection JSON.
ALTER TABLE enterprise_saml_connections ADD COLUMN sealed_alternate_key text NOT NULL DEFAULT '';
