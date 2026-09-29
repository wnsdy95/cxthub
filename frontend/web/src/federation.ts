export interface OIDCConnection {
 enterprise_id: string; domain: string; issuer: string; client_id: string;
 auth_method: 'client_secret_basic' | 'client_secret_post'; revision: string;
}
export interface OIDCView {
 available: boolean; configured: boolean; connection?: OIDCConnection;
 callback_uri: string; linked: boolean; verified_until?: string;
}
export interface OIDCInput {
 domain: string; issuer: string; client_id: string; client_secret: string;
 auth_method: OIDCConnection['auth_method']; revision: string;
}
export interface SAMLSigningRotation { id: string; state: 'prepared' | 'active'; certificate: string; created_at: string; activated_at?: string; verified_at?: string }
export interface SAMLCertificateInfo { fingerprint: string; not_before: string; not_after: string; status: 'valid' | 'expiring' | 'expired' | 'not_yet_valid' }
export interface SAMLSigningInput { revision: string; action: 'prepare' | 'activate' | 'cancel' | 'rollback' | 'retire'; trust_confirmed: boolean }
export interface SAMLConnection { enterprise_id: string; domain: string; issuer: string; revision: string; certificate: string; rotation?: SAMLSigningRotation }
export interface SAMLView { available: boolean; configured: boolean; connection?: SAMLConnection; entity_id: string; acs: string; linked: boolean; verified_until?: string; signing_certificate?: SAMLCertificateInfo; alternate_certificate?: SAMLCertificateInfo }
export interface SAMLInput { domain: string; metadata: string; revision: string }
export interface AssurancePolicy { enterprise_id: string; revision: string; max_age_hours: number }
export interface CredentialApprovalInput { protocol: 'oidc' | 'saml'; connection_revision: string; policy_revision: string }
export interface CredentialAssurance {
 id: string; kind: 'cli' | 'mcp'; label: string; hint?: string; created_at: string; expires_at: string;
 state: 'unapproved' | 'approved' | 'expired' | 'revoked' | 'verification_changed';
 protocol?: 'oidc' | 'saml'; authenticated_at?: string; verified_until?: string;
}
export interface CredentialAssurancesView {
 available: boolean; policy?: AssurancePolicy; browser_proof?: CredentialApprovalInput;
 approval_available_until?: string; credentials: CredentialAssurance[];
}
