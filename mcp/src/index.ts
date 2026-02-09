import http from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";
import * as grpc from "@grpc/grpc-js";
import * as protoLoader from "@grpc/proto-loader";

const addr = process.env.MCP_HTTP_ADDR || "0.0.0.0:3333";
const coreGrpc = process.env.CORE_GRPC_ADDR || "llmcore:9090";
const coreHttp = process.env.CORE_HTTP_URL || "http://llmcore:8080";
const protoPathEnv = process.env.PROTO_PATH;

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const protoPath = protoPathEnv || path.resolve(__dirname, "../proto/llm.proto");

const pkgDef = protoLoader.loadSync(protoPath, {
  keepCase: true,
  longs: String,
  enums: String,
  defaults: true,
  oneofs: true,
});
const grpcObj = grpc.loadPackageDefinition(pkgDef) as any;
const CoreClient = grpcObj.llmmcp.v1.Core;
const client = new CoreClient(coreGrpc, grpc.credentials.createInsecure());

function readBody(req: http.IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk) => chunks.push(chunk));
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf-8")));
    req.on("error", reject);
  });
}

function sendJson(res: http.ServerResponse, status: number, payload: unknown) {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(payload));
}

// Проксирование HTTP-запросов к llm-core
function proxyToCore(
  coreUrl: string,
  method: string,
  body: string | null,
  res: http.ServerResponse
) {
  const url = new URL(coreUrl);
  const opts: http.RequestOptions = {
    hostname: url.hostname,
    port: url.port,
    path: url.pathname + url.search,
    method,
    headers: { "content-type": "application/json" },
  };

  const proxyReq = http.request(opts, (proxyRes) => {
    const chunks: Buffer[] = [];
    proxyRes.on("data", (c) => chunks.push(c));
    proxyRes.on("end", () => {
      const data = Buffer.concat(chunks).toString("utf-8");
      res.writeHead(proxyRes.statusCode ?? 502, {
        "content-type": proxyRes.headers["content-type"] || "application/json",
      });
      res.end(data);
    });
  });

  proxyReq.on("error", (err) => {
    sendJson(res, 502, { error: `core unavailable: ${err.message}` });
  });

  if (body) proxyReq.write(body);
  proxyReq.end();
}

const server = http.createServer(async (req, res) => {
  // eslint-disable-next-line no-console
  console.log(`mcp http ${req.method} ${req.url}`);
  if (!req.url) {
    sendJson(res, 404, { error: "not_found" });
    return;
  }

  if (req.url === "/health") {
    sendJson(res, 200, { status: "ok" });
    return;
  }

  if (req.url === "/submit" && req.method === "POST") {
    const raw = await readBody(req);
    let body: any = {};
    if (raw) {
      try {
        body = JSON.parse(raw);
      } catch (err) {
        sendJson(res, 400, { error: "invalid_json" });
        return;
      }
    }
    const payloadJson = JSON.stringify(body.payload ?? {});
    client.SubmitJob(
      {
        kind: body.kind ?? "echo",
        payload_json: payloadJson,
        priority: body.priority ?? 0,
        source: body.source ?? "mcp",
        max_attempts: body.max_attempts ?? 3,
        deadline_at: body.deadline_at ?? "",
      },
      (err: grpc.ServiceError | null, resp: any) => {
        if (err) {
          sendJson(res, 500, { error: err.message });
          return;
        }
        sendJson(res, 202, { job_id: resp.job_id });
      }
    );
    return;
  }

  if (req.url.startsWith("/jobs/") && req.method === "GET") {
    const parts = req.url.split("/").filter(Boolean);
    const jobId = parts[1];
    const isStream = parts.length === 3 && parts[2] === "stream";
    if (!jobId) {
      sendJson(res, 404, { error: "not_found" });
      return;
    }

    if (isStream) {
      res.writeHead(200, {
        "content-type": "text/event-stream",
        "cache-control": "no-cache",
        connection: "keep-alive",
      });
      const stream = client.StreamJob({ job_id: jobId });
      stream.on("data", (event: any) => {
        res.write(`event: ${event.type}\n`);
        res.write(`data: ${event.data_json || "{}"}\n\n`);
      });
      stream.on("error", (err: grpc.ServiceError) => {
        res.write(`event: error\n`);
        res.write(`data: ${JSON.stringify({ error: err.message })}\n\n`);
        res.end();
      });
      stream.on("end", () => {
        res.end();
      });
      return;
    }

    client.GetJob({ job_id: jobId }, (err: grpc.ServiceError | null, resp: any) => {
      if (err) {
        sendJson(res, 500, { error: err.message });
        return;
      }
      sendJson(res, 200, resp.job || {});
    });
    return;
  }

  // Проксирование: умный LLM-роутинг через core
  if (req.url === "/llm/request" && req.method === "POST") {
    const raw = await readBody(req);
    proxyToCore(`${coreHttp}/v1/llm/request`, "POST", raw, res);
    return;
  }

  // Проксирование: дашборд
  if (req.url === "/dashboard" && req.method === "GET") {
    proxyToCore(`${coreHttp}/v1/dashboard`, "GET", null, res);
    return;
  }

  // Проксирование: статистика расходов
  if (req.url.startsWith("/costs/summary") && req.method === "GET") {
    const qs = req.url.includes("?") ? req.url.slice(req.url.indexOf("?")) : "";
    proxyToCore(`${coreHttp}/v1/costs/summary${qs}`, "GET", null, res);
    return;
  }

  // Проксирование: бенчмарки
  if (req.url.startsWith("/benchmarks") && req.method === "GET") {
    const qs = req.url.includes("?") ? req.url.slice(req.url.indexOf("?")) : "";
    proxyToCore(`${coreHttp}/v1/benchmarks${qs}`, "GET", null, res);
    return;
  }

  // Проксирование: discovery (устройства)
  if (req.url === "/discovery" && req.method === "GET") {
    proxyToCore(`${coreHttp}/v1/discovery`, "GET", null, res);
    return;
  }

  sendJson(res, 404, { error: "not_found" });
});

server.listen(Number(addr.split(":")[1] || 3333), addr.split(":")[0], () => {
  // eslint-disable-next-line no-console
  console.log(`mcp adapter listening on ${addr}, core grpc=${coreGrpc}`);
});
