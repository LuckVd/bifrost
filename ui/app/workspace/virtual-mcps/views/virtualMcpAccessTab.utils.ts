import type { VirtualKey } from "@/lib/types/governance";

/** The team, business unit, customer or user a virtual key belongs to, as the access tab names it. */
export interface VirtualKeyOwner {
	kind: "team" | "business unit" | "customer" | "user";
	name: string;
}

/**
 * Who a virtual key belongs to, so granting it access to a Virtual MCP can say whose key it is first:
 * the team, business unit or customer that owns it, or the user it is assigned to. A key has at most
 * one; a name falls back to the id (or a user's email) when it was not loaded with the key.
 */
export function virtualKeyOwner(
	vk:
		| Pick<VirtualKey, "team_id" | "team" | "business_unit_id" | "business_unit" | "customer_id" | "customer" | "assigned_user">
		| null
		| undefined,
): VirtualKeyOwner | undefined {
	if (vk?.team_id) return { kind: "team", name: vk.team?.name || vk.team_id };
	if (vk?.business_unit_id) return { kind: "business unit", name: vk.business_unit?.name || vk.business_unit_id };
	if (vk?.customer_id) return { kind: "customer", name: vk.customer?.name || vk.customer_id };
	if (vk?.assigned_user) return { kind: "user", name: vk.assigned_user.name || vk.assigned_user.email || vk.assigned_user.id };
	return undefined;
}