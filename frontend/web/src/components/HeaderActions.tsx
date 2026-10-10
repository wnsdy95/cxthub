import type { User } from '../types';
import { AccountMenu } from './AccountMenu';
import { CreateMenu } from './CreateMenu';

export function HeaderActions({ user }: { user: User }) {
  return <><CreateMenu /><AccountMenu user={user} /></>;
}
