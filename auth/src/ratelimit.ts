/**
 * A fixed-window counter per source address, for the one prefix better-auth's
 * own rate limiting does not cover.
 *
 * `/kitchen` is Kitchen's, mounted ahead of the better-auth catch-all, so
 * nothing in better-auth's limiter has ever seen a request to it. That was
 * survivable while the prefix was only a credential check; it is not the
 * thing to leave unmetered on a surface that mints CI keys and rewrites
 * redirect lists. The window is deliberately crude — a counter and a
 * timestamp per address, in the process's own memory — because the defence
 * that matters is the port split, and this is what keeps a caller that has
 * found the port from hammering it.
 *
 * Per replica, then, and not shared through Postgres: a limiter that needed a
 * round trip to the database on every request would be a new way for the
 * identity provider to fall over. Two replicas mean twice the ceiling, which
 * is the right trade for a bound that is about noise rather than about
 * correctness.
 */
export interface RateLimiter {
	/** Whether this request is allowed, counting it when it is. */
	allow(source: string): boolean;
}

/** A limiter that permits everything, for a configuration that asks for none. */
const unlimited: RateLimiter = { allow: () => true };

interface Window {
	/** When the current window started, in epoch milliseconds. */
	started: number;
	count: number;
}

/**
 * A limiter permitting `perMinute` requests from one source address per
 * minute. `perMinute` of 0 turns it off.
 *
 * `now` is injectable so a test can pin the window rather than sleep through
 * one.
 */
export function rateLimiter(perMinute: number, now: () => number = Date.now): RateLimiter {
	if (perMinute <= 0) {
		return unlimited;
	}

	const windowMillis = 60_000;
	/**
	 * Bounded, because the key is whatever address connected and an
	 * unbounded map keyed by that is a way to be run out of memory by the
	 * thing being limited. A full table is swept of everything whose window
	 * has passed, and only refuses to grow when even that leaves it full.
	 */
	const maxSources = 10_000;
	const windows = new Map<string, Window>();

	return {
		allow(source: string): boolean {
			const at = now();
			const current = windows.get(source);
			if (current && at - current.started < windowMillis) {
				current.count += 1;
				return current.count <= perMinute;
			}
			if (!current && windows.size >= maxSources) {
				for (const [key, window] of windows) {
					if (at - window.started >= windowMillis) {
						windows.delete(key);
					}
				}
				if (windows.size >= maxSources) {
					return false;
				}
			}
			windows.set(source, { started: at, count: 1 });
			return true;
		},
	};
}
