import { Button } from "@/components/ui/button";
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from "@/components/ui/form";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { getErrorMessage, setProviderFormDirtyState, useAppDispatch } from "@/lib/store";
import {
	useGetProviderOpenAPISpecQuery,
	usePreviewProviderOpenAPISpecMutation,
	useUpdateProviderMutation,
} from "@/lib/store/apis/providersApi";
import {
	GenericAuthTypeLabels,
	GenericAuthTypes,
	GenericOpenAPIPreviewResponse,
	GenericProviderConfig,
	GenericRouteMethods,
	GenericUnitsMeasureLabels,
	GenericUnitsSourceLabels,
	ModelProvider,
} from "@/lib/types/config";
import { genericProviderConfigSchema } from "@/lib/types/schemas";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import { zodResolver } from "@hookform/resolvers/zod";
import { Trash2 } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { useFieldArray, useForm } from "react-hook-form";
import { toast } from "sonner";
import { z } from "zod";
import { buildProviderUpdatePayload } from "../views/utils";

type FormValues = z.infer<typeof genericProviderConfigSchema>;

interface Props {
	provider: ModelProvider;
}

// A route's stream flag is tri-state: inherit the provider's detection, force streaming, or force buffering.
const STREAM_CHOICES = [
	{ value: "inherit", label: "Detect" },
	{ value: "stream", label: "Always stream" },
	{ value: "buffer", label: "Never stream" },
] as const;

const streamChoice = (stream?: boolean) => (stream === undefined ? "inherit" : stream ? "stream" : "buffer");
const streamFlag = (choice: string) => (choice === "inherit" ? undefined : choice === "stream");

const emptyUsage = {
	model_path: "",
	input_tokens_path: "",
	output_tokens_path: "",
	total_tokens_path: "",
	cost_path: "",
	cost_per_request: 0,
	units_path: "",
	units_source: "response" as const,
	units_measure: "value" as const,
	cost_per_unit: 0,
	disable_optimistic_parsing: false,
};

function toFormValues(cfg: GenericProviderConfig | undefined): FormValues {
	return {
		auth: {
			type: cfg?.auth?.type ?? "bearer",
			header_name: cfg?.auth?.header_name ?? "",
			query_param: cfg?.auth?.query_param ?? "",
			value_prefix: cfg?.auth?.value_prefix ?? "",
			aws: { service: cfg?.auth?.aws?.service ?? "", region: cfg?.auth?.aws?.region ?? "" },
		},
		usage: { ...emptyUsage, ...(cfg?.usage ?? {}) },
		streaming: { paths: cfg?.streaming?.paths ?? [], detect_from_accept_header: cfg?.streaming?.detect_from_accept_header ?? false },
		routes: (cfg?.routes ?? []).map((r) => ({ ...r, usage: { ...emptyUsage, cost_per_request: undefined, ...(r.usage ?? {}) } })),
		openapi: {
			spec: cfg?.openapi?.spec ?? "",
			spec_url: cfg?.openapi?.spec_url ?? "",
			validate_request_body: cfg?.openapi?.validate_request_body ?? false,
			enforce_operations: cfg?.openapi?.enforce_operations ?? false,
			spec_size: cfg?.openapi?.spec_size,
			spec_hash: cfg?.openapi?.spec_hash,
			spec_title: cfg?.openapi?.spec_title,
			openapi_version: cfg?.openapi?.openapi_version,
			operation_count: cfg?.openapi?.operation_count,
		},
	};
}

// Empty strings are form conveniences, not config: strip them so the stored block only carries
// what the operator set, and a usage block with nothing in it is dropped altogether.
// keepZeroFee is set for route usage: a route's explicit 0 waives the provider fee and must be
// sent, while the provider-level field treats 0 as "no fee" and is dropped.
function cleanUsage(usage: FormValues["usage"], keepZeroFee = false) {
	if (!usage) return undefined;
	const out: NonNullable<GenericProviderConfig["usage"]> = {};
	if (usage.model_path) out.model_path = usage.model_path;
	if (usage.input_tokens_path) out.input_tokens_path = usage.input_tokens_path;
	if (usage.output_tokens_path) out.output_tokens_path = usage.output_tokens_path;
	if (usage.total_tokens_path) out.total_tokens_path = usage.total_tokens_path;
	if (usage.cost_path) out.cost_path = usage.cost_path;
	if (usage.cost_per_request !== undefined && (keepZeroFee || usage.cost_per_request > 0)) out.cost_per_request = usage.cost_per_request;
	if (usage.cost_per_unit !== undefined && (keepZeroFee || usage.cost_per_unit > 0)) {
		out.cost_per_unit = usage.cost_per_unit;
		if (usage.units_path) out.units_path = usage.units_path;
		if (usage.units_source && usage.units_source !== "response") out.units_source = usage.units_source;
		if (usage.units_measure && usage.units_measure !== "value") out.units_measure = usage.units_measure;
	}
	if (usage.disable_optimistic_parsing) out.disable_optimistic_parsing = true;
	return Object.keys(out).length > 0 ? out : undefined;
}

function toConfig(values: FormValues): GenericProviderConfig {
	const auth = values.auth;
	const cfg: GenericProviderConfig = {
		auth: {
			type: auth.type,
			...(auth.type === "header" ? { header_name: auth.header_name, value_prefix: auth.value_prefix || undefined } : {}),
			...(auth.type === "query" ? { query_param: auth.query_param } : {}),
			...(auth.type === "aws_sigv4" ? { aws: { service: auth.aws?.service ?? "", region: auth.aws?.region || undefined } } : {}),
		},
	};
	const usage = cleanUsage(values.usage);
	if (usage) cfg.usage = usage;
	const paths = (values.streaming?.paths ?? []).filter((p) => p.trim() !== "");
	if (paths.length > 0 || values.streaming?.detect_from_accept_header) {
		cfg.streaming = {
			paths: paths.length > 0 ? paths : undefined,
			detect_from_accept_header: values.streaming?.detect_from_accept_header || undefined,
		};
	}
	if (values.routes && values.routes.length > 0) {
		cfg.routes = values.routes.map((r) => ({
			method: r.method,
			path: r.path,
			billable: r.billable === false ? false : undefined,
			stream: r.stream,
			usage: cleanUsage(r.usage, true),
		}));
	}
	const spec = values.openapi?.spec?.trim() ?? "";
	const specURL = values.openapi?.spec_url?.trim() ?? "";
	if (spec || specURL || values.openapi?.spec_hash) {
		// An unchanged document is sent back empty: the server keeps the stored one when the
		// spec_url is unchanged, so a save never has to carry a large document it did not edit.
		cfg.openapi = {
			spec: spec || undefined,
			spec_url: specURL || undefined,
			validate_request_body: values.openapi?.validate_request_body || undefined,
			enforce_operations: values.openapi?.enforce_operations || undefined,
		};
	}
	return cfg;
}

// GenericPassthroughFormFragment is the "Passthrough" tab of a generic relay provider: upstream
// auth, usage/cost mapping, stream detection, per-route overrides and the optional OpenAPI document.
export function GenericPassthroughFormFragment({ provider }: Props) {
	const hasUpdateProviderAccess = useRbac(RbacResource.ModelProvider, RbacOperation.Update);
	const dispatch = useAppDispatch();
	const [updateProvider, { isLoading: isUpdatingProvider }] = useUpdateProviderMutation();
	const [previewSpec, { isLoading: isPreviewing }] = usePreviewProviderOpenAPISpecMutation();
	const [preview, setPreview] = useState<GenericOpenAPIPreviewResponse | null>(null);
	const genericConfig = provider.custom_provider_config?.generic_config;
	const hasStoredSpec = !!genericConfig?.openapi?.spec_hash;
	// Provider reads leave the document text out; fetch it only when there is one to show.
	const { data: storedSpec } = useGetProviderOpenAPISpecQuery(provider.name, { skip: !hasStoredSpec });

	const form = useForm<FormValues>({
		resolver: zodResolver(genericProviderConfigSchema),
		mode: "onChange",
		defaultValues: toFormValues(genericConfig),
	});
	const routes = useFieldArray({ control: form.control, name: "routes" });

	useEffect(() => {
		dispatch(setProviderFormDirtyState(form.formState.isDirty));
	}, [form.formState.isDirty]);

	useEffect(() => {
		const values = toFormValues(genericConfig);
		if (storedSpec?.spec) {
			values.openapi = { ...values.openapi, spec: storedSpec.spec };
		}
		form.reset(values);
	}, [form, provider.name, genericConfig, storedSpec?.spec]);

	const authType = form.watch("auth.type");
	const specText = form.watch("openapi.spec");
	const specURL = form.watch("openapi.spec_url");
	const streamingPathsText = useMemo(() => (form.watch("streaming.paths") ?? []).join("\n"), [form.watch("streaming.paths")]);

	const onSubmit = (values: FormValues) => {
		updateProvider(
			buildProviderUpdatePayload(provider, {
				custom_provider_config: {
					...provider.custom_provider_config,
					base_provider_type: "generic",
					is_key_less: values.auth.type === "none" ? (provider.custom_provider_config?.is_key_less ?? false) : false,
					generic_config: toConfig(values),
				},
			}),
		)
			.unwrap()
			.then(() => {
				toast.success("Passthrough configuration updated successfully");
				form.reset(values);
			})
			.catch((err) => {
				toast.error("Failed to update passthrough configuration", { description: getErrorMessage(err) });
			});
	};

	const onPreview = () => {
		const body = specText?.trim() ? { spec: specText } : { spec_url: specURL };
		previewSpec(body)
			.unwrap()
			.then((result) => {
				setPreview(result);
				if (result.spec) {
					form.setValue("openapi.spec", result.spec, { shouldDirty: true });
					if (result.spec_url) form.setValue("openapi.spec_url", result.spec_url, { shouldDirty: true });
				}
			})
			.catch((err) => {
				setPreview(null);
				toast.error("Could not parse the OpenAPI document", { description: getErrorMessage(err) });
			});
	};

	const disabled = !hasUpdateProviderAccess;
	const textField = (
		name: Parameters<typeof form.register>[0],
		label: string,
		placeholder: string,
		testId: string,
		description?: string,
	) => (
		<FormField
			control={form.control}
			name={name}
			render={({ field }) => (
				<FormItem>
					<FormLabel>{label}</FormLabel>
					<FormControl>
						<Input
							placeholder={placeholder}
							data-testid={testId}
							disabled={disabled}
							{...field}
							value={(field.value as string | undefined) ?? ""}
						/>
					</FormControl>
					{description && <FormDescription>{description}</FormDescription>}
					<FormMessage />
				</FormItem>
			)}
		/>
	);
	const switchField = (name: Parameters<typeof form.register>[0], label: string, description: string, testId: string) => (
		<FormField
			control={form.control}
			name={name}
			render={({ field }) => (
				<FormItem>
					<div className="bg-muted/50 flex items-center justify-between space-x-2 rounded-sm border p-3">
						<div className="space-y-0.5">
							<p className="text-sm font-medium">{label}</p>
							<p className="text-muted-foreground text-sm">{description}</p>
						</div>
						<Switch size="md" checked={!!field.value} onCheckedChange={field.onChange} disabled={disabled} data-testid={testId} />
					</div>
				</FormItem>
			)}
		/>
	);

	return (
		<Form {...form}>
			<form onSubmit={form.handleSubmit(onSubmit)} className="space-y-8 px-4 pb-6 md:px-6" data-testid="generic-passthrough-form">
				<p className="text-muted-foreground text-sm">
					Requests to <code className="font-mono text-xs">/passthrough/{provider.name}/&lt;path&gt;</code> are relayed to{" "}
					<code className="font-mono text-xs">{provider.network_config?.base_url || "the configured base URL"}</code> with the selected key
					injected, and every call is logged, counted and charged.
				</p>

				{/* Auth */}
				<section className="space-y-4">
					<h3 className="text-sm font-semibold">Upstream authentication</h3>
					<FormField
						control={form.control}
						name="auth.type"
						render={({ field }) => (
							<FormItem>
								<FormLabel>Auth type</FormLabel>
								<Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
									<FormControl>
										<SelectTrigger data-testid="generic-auth-type-select">
											<SelectValue placeholder="Select auth type" />
										</SelectTrigger>
									</FormControl>
									<SelectContent>
										{GenericAuthTypes.map((t) => (
											<SelectItem key={t} value={t} data-testid={`generic-auth-type-option-${t}`}>
												{GenericAuthTypeLabels[t]}
											</SelectItem>
										))}
									</SelectContent>
								</Select>
								<FormMessage />
							</FormItem>
						)}
					/>
					{authType === "header" && textField("auth.header_name", "Header name", "x-api-key", "generic-auth-header-name-input")}
					{authType === "header" && textField("auth.value_prefix", "Value prefix (optional)", "Token ", "generic-auth-value-prefix-input")}
					{authType === "query" && textField("auth.query_param", "Query parameter", "api_key", "generic-auth-query-param-input")}
					{authType === "aws_sigv4" &&
						textField(
							"auth.aws.service",
							"AWS service",
							"textract",
							"generic-aws-service-input",
							"SigV4 signing name, e.g. textract or translate",
						)}
					{authType === "aws_sigv4" &&
						textField(
							"auth.aws.region",
							"AWS region (optional)",
							"us-east-1",
							"generic-aws-region-input",
							"Falls back to the key's bedrock_key_config.region",
						)}
				</section>

				{/* Usage mapping */}
				<section className="space-y-4">
					<h3 className="text-sm font-semibold">Usage and cost mapping</h3>
					<p className="text-muted-foreground text-sm">
						gjson paths read from the request (model) and the JSON response (tokens, cost). Left empty, Bifrost reads the common shapes:
						request <code className="font-mono text-xs">model</code>, response{" "}
						<code className="font-mono text-xs">usage.prompt_tokens|input_tokens</code>,{" "}
						<code className="font-mono text-xs">usage.completion_tokens|output_tokens</code>,{" "}
						<code className="font-mono text-xs">usage.total_tokens</code>, <code className="font-mono text-xs">usage.cost|cost</code>.
					</p>
					<div className="grid gap-4 md:grid-cols-2">
						{textField("usage.model_path", "Model path", "model", "generic-usage-model-path-input")}
						{textField("usage.cost_path", "Cost path", "usage.cost", "generic-usage-cost-path-input")}
						{textField("usage.input_tokens_path", "Input tokens path", "usage.prompt_tokens", "generic-usage-input-tokens-path-input")}
						{textField(
							"usage.output_tokens_path",
							"Output tokens path",
							"usage.completion_tokens",
							"generic-usage-output-tokens-path-input",
						)}
						{textField("usage.total_tokens_path", "Total tokens path", "usage.total_tokens", "generic-usage-total-tokens-path-input")}
						<FormField
							control={form.control}
							name="usage.cost_per_request"
							render={({ field }) => (
								<FormItem>
									<FormLabel>Cost per request (USD)</FormLabel>
									<FormControl>
										<Input
											type="number"
											step="any"
											min={0}
											placeholder="0"
											data-testid="generic-usage-cost-per-request-input"
											disabled={disabled}
											value={field.value ?? 0}
											onChange={(e) => field.onChange(e.target.value === "" ? 0 : Number(e.target.value))}
										/>
									</FormControl>
									<FormDescription>Flat fee added to every billable call, on top of any extracted cost</FormDescription>
									<FormMessage />
								</FormItem>
							)}
						/>
					</div>
					<div className="space-y-3 rounded-sm border p-3" data-testid="generic-units-section">
						<div>
							<p className="text-sm font-medium">Per-unit pricing</p>
							<p className="text-muted-foreground text-sm">
								For APIs that bill by quantity: pages the response reports, characters of the text you sent, or the size of the request
								body. Cost per call becomes units × rate, plus the flat fee and any extracted cost.
							</p>
						</div>
						<div className="grid gap-4 md:grid-cols-2">
							<FormField
								control={form.control}
								name="usage.units_source"
								render={({ field }) => (
									<FormItem>
										<FormLabel>Read the quantity from</FormLabel>
										<Select value={field.value ?? "response"} onValueChange={field.onChange} disabled={disabled}>
											<FormControl>
												<SelectTrigger data-testid="generic-units-source-select">
													<SelectValue />
												</SelectTrigger>
											</FormControl>
											<SelectContent>
												{(Object.keys(GenericUnitsSourceLabels) as (keyof typeof GenericUnitsSourceLabels)[]).map((k) => (
													<SelectItem key={k} value={k}>
														{GenericUnitsSourceLabels[k]}
													</SelectItem>
												))}
											</SelectContent>
										</Select>
									</FormItem>
								)}
							/>
							<FormField
								control={form.control}
								name="usage.units_measure"
								render={({ field }) => (
									<FormItem>
										<FormLabel>Measure</FormLabel>
										<Select value={field.value ?? "value"} onValueChange={field.onChange} disabled={disabled}>
											<FormControl>
												<SelectTrigger data-testid="generic-units-measure-select">
													<SelectValue />
												</SelectTrigger>
											</FormControl>
											<SelectContent>
												{(Object.keys(GenericUnitsMeasureLabels) as (keyof typeof GenericUnitsMeasureLabels)[]).map((k) => (
													<SelectItem key={k} value={k}>
														{GenericUnitsMeasureLabels[k]}
													</SelectItem>
												))}
											</SelectContent>
										</Select>
									</FormItem>
								)}
							/>
							{form.watch("usage.units_measure") !== "body_bytes" &&
								textField(
									"usage.units_path",
									"Units path",
									"DocumentMetadata.Pages",
									"generic-units-path-input",
									"e.g. DocumentMetadata.Pages (Textract), usage.credits (Tavily), Text (Translate)",
								)}
							<FormField
								control={form.control}
								name="usage.cost_per_unit"
								render={({ field }) => (
									<FormItem>
										<FormLabel>Cost per unit (USD)</FormLabel>
										<FormControl>
											<Input
												type="number"
												step="any"
												min={0}
												placeholder="0"
												data-testid="generic-units-cost-per-unit-input"
												disabled={disabled}
												value={field.value ?? 0}
												onChange={(e) => field.onChange(e.target.value === "" ? 0 : Number(e.target.value))}
											/>
										</FormControl>
										<FormDescription>e.g. 0.0015 per Textract page, 0.000015 per Translate character</FormDescription>
										<FormMessage />
									</FormItem>
								)}
							/>
						</div>
					</div>
					{switchField(
						"usage.disable_optimistic_parsing",
						"Only read the configured paths",
						"Turn off the common-shape fallbacks so nothing is inferred from a response you did not map",
						"generic-usage-disable-optimistic-switch",
					)}
				</section>

				{/* Streaming */}
				<section className="space-y-4">
					<h3 className="text-sm font-semibold">Streaming</h3>
					<FormItem>
						<FormLabel>Streaming paths</FormLabel>
						<FormControl>
							<Textarea
								placeholder={"/v1/stream/**\n/events/*"}
								data-testid="generic-streaming-paths-textarea"
								disabled={disabled}
								value={streamingPathsText}
								onChange={(e) =>
									form.setValue(
										"streaming.paths",
										e.target.value.split("\n").map((l) => l.trim()),
										{ shouldDirty: true },
									)
								}
								rows={3}
							/>
						</FormControl>
						<FormDescription>
							One rooted glob per line; a trailing /** matches everything below. These routes are always served as streams.
						</FormDescription>
					</FormItem>
					{switchField(
						"streaming.detect_from_accept_header",
						"Stream on Accept: text/event-stream",
						"Treat a request whose Accept header asks for SSE as a streaming request",
						"generic-streaming-accept-switch",
					)}
				</section>

				{/* Routes */}
				<section className="space-y-4">
					<div className="flex items-center justify-between">
						<div>
							<h3 className="text-sm font-semibold">Routes</h3>
							<p className="text-muted-foreground text-sm">
								Per-operation overrides; the first matching route wins. Non-billable routes are relayed but not counted.
							</p>
						</div>
						<Button
							type="button"
							variant="outline"
							size="sm"
							disabled={disabled}
							data-testid="generic-route-add-btn"
							onClick={() =>
								routes.append({
									method: "POST",
									path: "/",
									billable: true,
									stream: undefined,
									usage: { ...emptyUsage, cost_per_request: undefined },
								})
							}
						>
							Add route
						</Button>
					</div>
					{routes.fields.map((route, index) => (
						<div key={route.id} className="space-y-3 rounded-sm border p-3" data-testid={`generic-route-row-${index}`}>
							<div className="grid gap-3 md:grid-cols-[120px_1fr_140px_auto]">
								<FormField
									control={form.control}
									name={`routes.${index}.method`}
									render={({ field }) => (
										<FormItem>
											<Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
												<FormControl>
													<SelectTrigger data-testid={`generic-route-method-select-${index}`}>
														<SelectValue />
													</SelectTrigger>
												</FormControl>
												<SelectContent>
													{GenericRouteMethods.map((m) => (
														<SelectItem key={m} value={m}>
															{m}
														</SelectItem>
													))}
												</SelectContent>
											</Select>
										</FormItem>
									)}
								/>
								<FormField
									control={form.control}
									name={`routes.${index}.path`}
									render={({ field }) => (
										<FormItem>
											<FormControl>
												<Input placeholder="/search" data-testid={`generic-route-path-input-${index}`} disabled={disabled} {...field} />
											</FormControl>
											<FormMessage />
										</FormItem>
									)}
								/>
								<FormField
									control={form.control}
									name={`routes.${index}.stream`}
									render={({ field }) => (
										<FormItem>
											<Select value={streamChoice(field.value)} onValueChange={(v) => field.onChange(streamFlag(v))} disabled={disabled}>
												<FormControl>
													<SelectTrigger data-testid={`generic-route-stream-select-${index}`}>
														<SelectValue />
													</SelectTrigger>
												</FormControl>
												<SelectContent>
													{STREAM_CHOICES.map((c) => (
														<SelectItem key={c.value} value={c.value}>
															{c.label}
														</SelectItem>
													))}
												</SelectContent>
											</Select>
										</FormItem>
									)}
								/>
								<Button
									type="button"
									variant="ghost"
									size="icon"
									disabled={disabled}
									data-testid={`generic-route-remove-btn-${index}`}
									onClick={() => routes.remove(index)}
								>
									<Trash2 className="h-4 w-4" />
								</Button>
							</div>
							<div className="grid gap-3 md:grid-cols-2">
								<FormField
									control={form.control}
									name={`routes.${index}.billable`}
									render={({ field }) => (
										<FormItem>
											<div className="bg-muted/50 flex items-center justify-between space-x-2 rounded-sm border p-3">
												<p className="text-sm font-medium">Billable</p>
												<Switch
													size="md"
													checked={field.value !== false}
													onCheckedChange={field.onChange}
													disabled={disabled}
													data-testid={`generic-route-billable-switch-${index}`}
												/>
											</div>
										</FormItem>
									)}
								/>
								<FormField
									control={form.control}
									name={`routes.${index}.usage.cost_per_request`}
									render={({ field }) => (
										<FormItem>
											<FormControl>
												<Input
													type="number"
													step="any"
													min={0}
													placeholder="Cost per request override (USD); empty inherits, 0 waives the fee"
													data-testid={`generic-route-cost-per-request-input-${index}`}
													disabled={disabled}
													value={field.value ?? ""}
													onChange={(e) => field.onChange(e.target.value === "" ? undefined : Number(e.target.value))}
												/>
											</FormControl>
										</FormItem>
									)}
								/>
							</div>
						</div>
					))}
				</section>

				{/* OpenAPI */}
				<section className="space-y-4">
					<h3 className="text-sm font-semibold">OpenAPI document (optional)</h3>
					<p className="text-muted-foreground text-sm">
						Paste the API's OpenAPI or Swagger document, or give its URL, to refuse operations it does not list and request bodies that fail
						its schemas.
					</p>
					{textField("openapi.spec_url", "Spec URL", "https://api.example.com/openapi.json", "generic-openapi-spec-url-input")}
					<FormField
						control={form.control}
						name="openapi.spec"
						render={({ field }) => (
							<FormItem>
								<FormLabel>Spec (JSON or YAML)</FormLabel>
								<FormControl>
									<Textarea
										placeholder="openapi: 3.0.0 …"
										data-testid="generic-openapi-spec-textarea"
										disabled={disabled}
										rows={8}
										{...field}
										value={field.value ?? ""}
									/>
								</FormControl>
								<FormMessage />
							</FormItem>
						)}
					/>
					<div className="flex items-center gap-3">
						<Button
							type="button"
							variant="outline"
							size="sm"
							isLoading={isPreviewing}
							disabled={disabled || (!specText?.trim() && !specURL?.trim())}
							data-testid="generic-openapi-preview-btn"
							onClick={onPreview}
						>
							Parse document
						</Button>
						{(preview || hasStoredSpec) && (
							<p className="text-muted-foreground text-xs" data-testid="generic-openapi-summary">
								{preview
									? `${preview.title || "Untitled"} ${preview.version ? `v${preview.version}` : ""} (OpenAPI ${preview.openapi_version}) — ${preview.operation_count} operations`
									: `${genericConfig?.openapi?.spec_title || "Document"} (OpenAPI ${genericConfig?.openapi?.openapi_version}) — ${genericConfig?.openapi?.operation_count} operations stored`}
							</p>
						)}
					</div>
					{preview && preview.operations.length > 0 && (
						<ul className="max-h-48 space-y-1 overflow-y-auto rounded-sm border p-2 text-xs" data-testid="generic-openapi-operations">
							{preview.operations.map((op) => (
								<li key={`${op.method} ${op.path}`} className="flex items-center justify-between gap-2 font-mono">
									<span>
										{op.method} {op.path}
									</span>
									<Button
										type="button"
										variant="ghost"
										size="sm"
										disabled={disabled}
										onClick={() =>
											routes.append({
												method: op.method as (typeof GenericRouteMethods)[number],
												path: op.path.replace(/\{[^}]+\}/g, "*"),
												billable: true,
												stream: undefined,
												usage: { ...emptyUsage },
											})
										}
									>
										Add as route
									</Button>
								</li>
							))}
						</ul>
					)}
					{switchField(
						"openapi.enforce_operations",
						"Enforce operations",
						"Refuse method and path pairs the document does not list (404)",
						"generic-openapi-enforce-operations-switch",
					)}
					{switchField(
						"openapi.validate_request_body",
						"Validate request bodies",
						"Refuse JSON bodies that fail the matched operation's schema (400)",
						"generic-openapi-validate-body-switch",
					)}
				</section>

				<div className="flex justify-end">
					<Button
						type="submit"
						isLoading={isUpdatingProvider}
						disabled={disabled || !form.formState.isDirty}
						data-testid="generic-passthrough-save-btn"
					>
						Save
					</Button>
				</div>
			</form>
		</Form>
	);
}