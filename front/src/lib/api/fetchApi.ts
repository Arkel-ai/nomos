const API_BASE = '/api';

interface ApiResponse<T = any> {
	status: number;
	data: T;
}

/**
 * Thin fetch wrapper that always hits the same-origin `/api` proxy.
 *
 * Auth is cookie-based: Better Auth sets the `better-auth.session_token`
 * cookie on login, and `credentials: 'include'` makes the browser send
 * it automatically. The Go backend validates the session by looking the
 * token up in Postgres (see api/middlewares/auth.go) and exposes the
 * user via `middlewares.CurrentUser(c)`.
 */
export async function fetchApi<T = any>(
	path: string,
	method: string = 'GET',
	body?: any
): Promise<ApiResponse<T>> {
	const headers: Record<string, string> = {};

	if (body && !(body instanceof FormData)) {
		headers['Content-Type'] = 'application/json';
		body = JSON.stringify(body);
	}

	const response = await fetch(`${API_BASE}${path}`, {
		method,
		headers,
		body,
		credentials: 'include'
	});

	const data = await response.json();

	return {
		status: response.status,
		data
	};
}
