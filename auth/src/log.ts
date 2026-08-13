/**
 * Structured logs on stdout, the shape the platform's log collector expects.
 */
type Level = "debug" | "info" | "warn" | "error";

const LEVELS: Record<Level, number> = { debug: 10, info: 20, warn: 30, error: 40 };

function threshold(): number {
	const configured = (process.env.LOG_LEVEL ?? "info").toLowerCase() as Level;
	return LEVELS[configured] ?? LEVELS.info;
}

function emit(level: Level, message: string, fields?: Record<string, unknown>): void {
	if (LEVELS[level] < threshold()) {
		return;
	}
	const line = JSON.stringify({
		ts: new Date().toISOString(),
		level,
		msg: message,
		...fields,
	});
	if (level === "error" || level === "warn") {
		process.stderr.write(`${line}\n`);
	} else {
		process.stdout.write(`${line}\n`);
	}
}

export const log = {
	debug: (message: string, fields?: Record<string, unknown>) => emit("debug", message, fields),
	info: (message: string, fields?: Record<string, unknown>) => emit("info", message, fields),
	warn: (message: string, fields?: Record<string, unknown>) => emit("warn", message, fields),
	error: (message: string, fields?: Record<string, unknown>) => emit("error", message, fields),
};
