// oozie approval gate — loaded via `pi -e` for untrusted projects only.
//
// pi's --approve/--no-approve flags control whether project-local config
// files are trusted; they do NOT gate tool execution. This extension is
// what makes an untrusted project actually safe: every mutating built-in
// tool call (write, edit, bash) blocks on a confirm dialog, which reaches
// oozie over RPC as an extension_ui_request and renders in the permission
// panel. Deny blocks the tool; the model sees the refusal and continues.
export default function (pi: any) {
	const gated: Record<string, string> = {
		bash: "command",
		write: "path",
		edit: "path",
	};

	pi.on("tool_call", async (event: any, ctx: any) => {
		const detailKey = gated[event.toolName];
		if (detailKey === undefined) return;

		let detail = "";
		const value = event.input?.[detailKey];
		if (typeof value === "string") detail = value;
		if (detail.length > 300) detail = detail.slice(0, 297) + "…";

		const ok = await ctx.ui.confirm(
			`Allow ${event.toolName} in this untrusted project?`,
			detail || "(no detail provided)",
		);
		if (!ok) {
			return { block: true, reason: "The user denied this action in oozie's permission panel." };
		}
	});
}
