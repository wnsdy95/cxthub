export interface EnterpriseDomain {
 enterprise_id: string;
 domain: string;
 challenge: string;
 revision: string;
 challenge_expires_at: string;
 verified_at: string;
 verified_until: string;
 record_name: string;
 state: 'pending' | 'verified' | 'expired';
}
