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
