import { fetchApi } from './fetchApi';

export interface AdminUser {
	id: string;
	email: string;
	name: string;
	email_verified: boolean;
	roles: string[];
}

/** Admin: list every user with their roles. */
export const listUsers = () => fetchApi<{ users: AdminUser[]; detail?: string }>('/admin/users');

/** Admin: replace a user's role (admin | user). */
export const setUserRole = (userId: string, role: string) =>
	fetchApi<{ user_id: string; role: string; detail?: string }>(
		`/admin/users/${userId}/role`,
		'PUT',
		{ role }
	);
