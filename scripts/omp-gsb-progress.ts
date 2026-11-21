import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

const widgetKey = "gsb-wait";

export default function gsbProgress(pi: ExtensionAPI): void {
	let stopWaiting: (() => void) | undefined;

	function clear(): void {
		stopWaiting?.();
		stopWaiting = undefined;
	}

	pi.on("turn_start", (_event, ctx) => {
		clear();
		// 9router exposes GSB models with a gsb/ prefix; a direct provider can
		// register under the name gsb. Leave other routes untouched.
		if (!ctx.hasUI || (ctx.agent && ctx.agent.kind !== "main")) return;
		if (ctx.model?.provider !== "gsb" && !ctx.model?.id.startsWith("gsb/")) return;

		const startedAt = performance.now();
		const update = () => {
			const seconds = Math.floor((performance.now() - startedAt) / 1000);
			ctx.ui.setWidget(widgetKey, [`GSB: menunggu output pertama (${seconds} dtk)`]);
		};
		const timer = setInterval(update, 1000);
		timer.unref();
		stopWaiting = () => {
			clearInterval(timer);
			ctx.ui.setWidget(widgetKey, undefined);
		};
		update();
	});

	pi.on("message_update", ({ assistantMessageEvent: event }) => {
		if (
			((event.type === "thinking_delta" || event.type === "text_delta") && event.delta.trim() !== "") ||
			event.type === "toolcall_start" || event.type === "image_end"
		) clear();
	});
	pi.on("message_end", ({ message }) => {
		if (message.role === "assistant") clear();
	});
	pi.on("turn_end", clear);
	pi.on("agent_end", clear);
	pi.on("session_switch", clear);
	pi.on("session_shutdown", clear);
}
