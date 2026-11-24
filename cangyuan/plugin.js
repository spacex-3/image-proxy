// Image-only snapshot. It is intentionally excluded from the AZ proxy build.
const IMAGE_MODELS = [
  "gpt-image-2", "gpt-image-2-1k", "gpt-image-2-2k", "gpt-image-2-4k", "gpt-image-2-x",
  "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-flare-1k", "gpt-image-2.5-flare-2k", "gpt-image-2.5-flare-4k",
  "gpt-image-2.5-sunburst", "gpt-image-2.5-sunburst-1k", "gpt-image-2.5-sunburst-2k", "gpt-image-2.5-sunburst-4k", "gpt-image-2.5-x",
  "gpt-image-2.6", "gpt-image-2.6-flare-1k", "gpt-image-2.6-flare-2k", "gpt-image-2.6-flare-4k",
  "gpt-image-2.6-sunburst-1k", "gpt-image-2.6-sunburst-2k", "gpt-image-2.6-sunburst-4k",
  "gpt-image-2-az", "gpt-image-2.5-flare-az", "gpt-image-2.5-sunburst-az",
  "gpt-image-2-o", "gpt-image-2.5-flare-o", "gpt-image-2.5-sunburst-o", "gpt-image-2.6-flare-o", "gpt-image-2.6-sunburst-o",
  "nano-banana-pro-1k", "nano-banana-pro-2k", "nano-banana-pro-4k", "gemini-3-pro-image-preview",
  "nano-banana2-1k", "nano-banana2-2k", "nano-banana2-4k", "gemini-3.1-flash-image-preview", "gemini-nano-banana-2.1",
  "mj2-midjourney", "mj2-midjourney-blend", "mj2-midjourney-describe", "midjourney", "midjourney-1k", "midjourney-2k",
  "grok-imagine-image", "grok-imagine-image-2.0", "grok-imagine-image-lite",
  "doubao-seedream-5-0-flash", "seedream-5.0-pro-x", "doubao-seedream-5-0-pro", "doubao-seedream-5-0-lite",
  "doubao-seedream-5-0-lite-x", "doubao-seedream-5-0-pro-x", "Z-Image-Turbo", "flux-pro-2", "flux-3-image", "flux-3-klein", "mu1-muse-image",
];

const PER_IMAGE_MODELS = [
  "nano-banana2-4k", "gpt-image-2-1k", "nano-banana2-2k", "doubao-seedream-5-0-flash",
  "nano-banana-pro-1k", "nano-banana2-1k", "gpt-image-2", "gpt-image-2-4k",
  "nano-banana-pro-4k", "gpt-image-2-2k", "gpt-image-2.6", "gpt-image-2.5-sunburst",
  "doubao-seedream-5-0-pro", "gpt-image-2.5", "gpt-image-2.5-flare", "nano-banana-pro-2k",
];

export const meta = {
  apiVersion: 1,
  key: "cangyuan",
  name: "渠道 1",
  description: {
    en: "Asynchronous image tasks through a Cangyuan New API upstream",
    zh: "通过沧元 New API 上游调用异步图片任务",
  },
  version: "1.1.2",
  author: { name: "QuantumNous" },
  models: IMAGE_MODELS,
  fetchMode: "per_task",
  upstreams: ["new_api"],
  usageSchema: {
    n: { type: "number", unit: "count", description: { en: "Image generation unit price", zh: "图片生成单价" } },
    img_n: { type: "number", unit: "count", description: { en: "Output image unit price", zh: "输出图片单价" } },
    tin: { type: "number", unit: "token", description: { en: "Input text token unit price", zh: "输入文本 Token 单价" } },
    iin: { type: "number", unit: "token", description: { en: "Input image token unit price", zh: "输入图片 Token 单价" } },
    iout: { type: "number", unit: "token", description: { en: "Output image token unit price", zh: "输出图片 Token 单价" } },
    tokens: { type: "number", unit: "token", description: { en: "Upstream token unit price", zh: "上游 Token 单价" } },
    ref: { type: "number", unit: "count", description: { en: "Reference image unit price", zh: "参考图片单价" } },
  },
  usageExamples: [
    { label: "default image task", facts: { n: 1, img_n: 1, tin: 0, iin: 0, iout: 0, tokens: 0, ref: 0 } },
  ],
  routes: [
    { method: "GET", path: "/v1/images/generations/:task_id", type: "query", render: "imageTask" },
    { method: "GET", path: "/v1/images/edits/:task_id", type: "query", render: "imageTask" },
  ],
  protocols: [{ name: "openai_image", models: IMAGE_MODELS, supports: ["sync", "background"] }],
};

function trimmed(value) {
  return String(value || "").trim();
}

function object(value) {
  return value && typeof value === "object" && !Array.isArray(value) ? value : {};
}

function cangyuanBaseURL(value) {
  const base = trimmed(value).replace(/\/+$/, "");
  if (!/^https:\/\/([a-z0-9-]+\.)*cangyuansuanli\.cn(?::[0-9]+)?$/i.test(base)) {
    throw new Error("Cangyuan plugin requires an HTTPS cangyuansuanli.cn channel base URL");
  }
  return base;
}

const MEDIA_URL_FIELDS = [
  "image", "images", "mask", "reference", "image_url", "reference_image_url", "reference_image_urls",
];

function requireHTTPSMedia(req) {
  for (const key of MEDIA_URL_FIELDS) {
    if (req[key] === undefined || req[key] === null) continue;
    const values = Array.isArray(req[key]) ? req[key] : [req[key]];
    for (const item of values) {
      const url = typeof item === "string" ? item : object(item).url;
      if (!trimmed(url).startsWith("https://")) throw new Error(key + " must contain public HTTPS URLs");
    }
  }
}

function statusName(status) {
  const names = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "in_progress", SUCCESS: "completed", FAILURE: "failed" };
  return names[String(status || "").toUpperCase()] || "queued";
}

function publicTask(ctx, task) {
  const data = object(task.data);
  const output = Object.assign({}, data, {
    id: task.task_id,
    status: statusName(task.status),
    progress: data.progress === undefined ? task.progress : data.progress,
  });
  if (Array.isArray(data.data)) output.data = publicImageData(ctx, data.data);
  if (data.task_id !== undefined) output.task_id = task.task_id;
  if (task.status === "FAILURE") output.error = object(data.error).message ? data.error : { message: task.fail_reason || "task failed" };
  return output;
}

function publicImageData(ctx, data) {
  if (!ctx || !ctx.artifacts) return data;
  return data.map(function (item, index) {
    const artifact = ctx.artifacts["image-" + (index + 1)];
    return artifact && artifact.url ? Object.assign({}, object(item), { url: artifact.url }) : item;
  });
}

export const native = {
  imageTask: function (ctx, task) { return publicTask(ctx, task); },
  error: function (ctx, error) { return { error: { code: error.code, message: error.message } }; },
};

function endpointForAction(action) {
  if (action === "image_edit") return "/v1/images/edits";
  if (action === "image_generation") return "/v1/images/generations";
  throw new Error("unsupported Cangyuan image action");
}

export function buildSubmitRequest(ctx) {
  const body = Object.assign({}, ctx.requestBody || {}, { model: ctx.upstreamModel || ctx.model });
  body["async"] = true;
  return {
    url: cangyuanBaseURL(ctx.baseUrl) + endpointForAction(ctx.action),
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: "Bearer " + ctx.apiKey },
    body: body,
    action: ctx.action,
  };
}

export function parseSubmitResponse(ctx, resp) {
  const body = object(resp.body);
  const taskId = trimmed(body.id || body.task_id);
  if (!taskId) throw new Error("task_id is empty");
  return { taskId: taskId, taskData: body };
}

export function buildQueryRequest(ctx) {
  return {
    url: cangyuanBaseURL(ctx.baseUrl) + endpointForAction(ctx.action) + "/" + encodeURIComponent(ctx.taskId),
    method: "GET",
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
}

export function parseTaskResult(ctx, body) {
  body = object(body);
  const states = { queued: "QUEUED", pending: "QUEUED", processing: "IN_PROGRESS", in_progress: "IN_PROGRESS", completed: "SUCCESS", failed: "FAILURE", cancelled: "FAILURE" };
  const status = states[String(body.status || "").toLowerCase()] || "UNKNOWN";
  const result = { status: status };
  if (body.progress !== undefined) result.progress = String(body.progress);
  if (status === "FAILURE") result.reason = object(body.error).message || body.fail_reason || "task failed";
  if (status === "UNKNOWN") result.reason = "unrecognized status: " + String(body.status || "");
  return result;
}

export function listArtifacts(task) {
  if (task.status !== "SUCCESS") return [];
  const data = object(task.data);
  if (Array.isArray(data.data)) {
    const images = data.data.filter(function (item) {
      return trimmed(object(item).url).startsWith("https://");
    });
    if (images.length > 0) {
      return images.map(function (_, index) { return { key: "image-" + (index + 1), type: "image" }; });
    }
  }
  return [];
}

export function buildContentRequest(ctx) {
  const data = object(ctx.data);
  const match = /^image-([1-9][0-9]*)$/.exec(String(ctx.artifactKey || ""));
  if (match && Array.isArray(data.data)) {
    const images = data.data.filter(function (item) {
      return trimmed(object(item).url).startsWith("https://");
    });
    const url = trimmed(object(images[Number(match[1]) - 1]).url);
    if (!url) throw new Error("artifact_not_found");
    return { url: url, method: ctx.clientRequest.method, credentialless: true };
  }
  throw new Error("artifact_not_found");
}

function boundedCount(value, fallback) {
  const number = Number(value);
  return Number.isInteger(number) && number >= 1 && number <= 128 ? number : fallback;
}

function references(req, key) {
  return Array.isArray(req[key]) ? req[key].length : req[key] ? 1 : 0;
}

export function extractUsage(ctx) {
  const req = object(ctx.requestBody);
  const n = boundedCount(req.n, 1);
  if (ctx.usagePurpose === "billing_ratios") {
    const model = trimmed(ctx.upstreamModel || ctx.model || req.model);
    return PER_IMAGE_MODELS.includes(model) ? { n: n } : {};
  }
  const facts = {
    n: n,
    img_n: n,
    ref: references(req, "images") + references(req, "reference") + references(req, "reference_image_urls"),
    tin: 0,
    iin: 0,
    iout: 0,
    tokens: 0,
  };
  for (const key of ["series", "tier", "quality", "size", "resolution", "speed", "type", "texture", "personaModel"]) {
    if (["string", "number", "boolean"].includes(typeof req[key])) facts[key] = req[key];
  }
  facts.prompt = typeof req.prompt === "string" && req.prompt.includes("--fast") ? "--fast" : "";
  return facts;
}

export function extractUsageOnComplete(task, taskResult, body) {
  body = object(body);
  const usage = object(body.usage);
  const input = object(usage.input_tokens_details || usage.prompt_tokens_details);
  const output = object(usage.output_tokens_details || usage.completion_tokens_details);
  const facts = {};
  const values = {
    tin: input.text_tokens,
    iin: input.image_tokens,
    iout: output.image_tokens === undefined ? usage.output_tokens : output.image_tokens,
    tokens: usage.completion_tokens === undefined ? usage.total_tokens : usage.completion_tokens,
  };
  for (const key of Object.keys(values)) {
    const number = Number(values[key]);
    if (Number.isFinite(number) && number >= 0) facts[key] = number;
  }
  if (Array.isArray(body.data) && body.data.length > 0 && body.data.length <= 128) facts.img_n = body.data.length;
  return facts;
}

export const protocols = {
  openai_image: {
    decodeRequest: function (ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("Cangyuan image requests require JSON and HTTPS media URLs");
      const body = ctx.body.value;
      if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("request body must be an object");
      requireHTTPSMedia(body);
      const model = trimmed(ctx.model);
      if (!model) throw new Error("model is required");
      const n = boundedCount(body.n, 1);
      if (body.n !== undefined && Number(body.n) !== n) throw new Error("n must be an integer between 1 and 128");
      return {
        kind: "submit",
        model: model,
        action: ctx.operation === "edit" ? "image_edit" : "image_generation",
        requestBody: Object.assign({}, body, { model: model, n: n, "async": true }),
      };
    },
    render: function (ctx, task) {
      if (ctx.body.value["async"] === true) return publicTask(ctx, task);
      if (task.status !== "SUCCESS") return publicTask(ctx, task);
      const data = object(task.data);
      return { created: data.created || task.created_at, data: Array.isArray(data.data) ? publicImageData(ctx, data.data) : [], usage: data.usage };
    },
  },
};
