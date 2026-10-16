import { fetchApi } from './fetchApi';

export type InviteStatus = 'pending' | 'accepted' | 'revoked';

export interface Invitation {
	id: string;
	email: string;
	role: string;
	status: InviteStatus;
	invited_by: string;
	accepted_by?: string;
	expires_at: number;
	created_at: number;
	accepted_at?: number;
}

export interface CreatedInvitation {
	id: string;
	email: string;
	role: string;
	status: InviteStatus;
	expires_at: number;
	link: string;
}

export interface InviteValidation {
	valid: boolean;
	email?: string;
	role?: string;
	reason?: string;
}

/** Admin: list every invitation, newest first. */
export const listInvitations = () => fetchApi<{ invitations: Invitation[] }>('/invitations');

/** Admin: create an invitation; the response carries the one-time link. */
export const createInvitation = (email: string, role: string) =>
	fetchApi<CreatedInvitation & { detail?: string }>('/invitations', 'POST', { email, role });

/** Admin: revoke a pending invitation. */
export const revokeInvitation = (id: string) =>
	fetchApi<{ id: string; status: string; detail?: string }>(`/invitations/${id}`, 'DELETE');

/** Public: check a token and (when valid) get the email it was issued to. */
export const validateInvitation = (token: string) =>
	fetchApi<InviteValidation>(`/invitations/validate?token=${encodeURIComponent(token)}`);

/** Redeem a token as the just-created, authenticated user. */
export const acceptInvitation = (token: string) =>
	fetchApi<{ role?: string; detail?: string }>('/invitations/accept', 'POST', { token });
