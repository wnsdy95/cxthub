import { useT, type MsgKey } from '../i18n';
import { atLeast, ROLE_CAPABILITIES, ROLES, type RoleCapability } from '../roles';

const CAPABILITY_KEYS: Record<RoleCapability, MsgKey> = {
  viewContext: 'settings.capabilityViewContext',
  pullTeamAssets: 'settings.capabilityPullTeamAssets',
  pushContext: 'settings.capabilityPushContext',
  manageTeamAssets: 'settings.capabilityManageTeamAssets',
  administerWorkspace: 'settings.capabilityAdministerWorkspace',
};

export function RoleCapabilities() {
  const t = useT();
  return (
    <section className="role-capabilities" aria-labelledby="role-capabilities-title">
      <div className="role-capabilities-head">
        <span className="label" id="role-capabilities-title">{t('settings.roleCapabilitiesTitle')}</span>
        <span className="role-ladder" aria-label={t('settings.roleLadder')}>
          viewer <span aria-hidden="true">→</span> owner
        </span>
      </div>
      <p className="hint">{t('settings.roleCapabilitiesHint')}</p>
      <div className="role-capabilities-scroll" tabIndex={0} aria-label={t('settings.roleCapabilitiesTable')}>
        <table>
          <thead>
            <tr>
              <th scope="col">{t('settings.capabilityColumn')}</th>
              {ROLES.map((role) => (
                <th scope="col" key={role} title={t(`roles.${role}` as MsgKey)}>
                  <code>{role}</code>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {ROLE_CAPABILITIES.map((capability) => {
              const label = t(CAPABILITY_KEYS[capability.id]);
              return (
                <tr key={capability.id}>
                  <th scope="row">{label}</th>
                  {ROLES.map((role) => {
                    const allowed = atLeast(role, capability.minimumRole);
                    return (
                      <td
                        key={role}
                        className={allowed ? 'allowed' : 'denied'}
                        aria-label={t(allowed ? 'settings.capabilityAllowed' : 'settings.capabilityDenied', {
                          role,
                          capability: label,
                        })}
                      >
                        <span aria-hidden="true">{allowed ? '✓' : '—'}</span>
                      </td>
                    );
                  })}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </section>
  );
}
