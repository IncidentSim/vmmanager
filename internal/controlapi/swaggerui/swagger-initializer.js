// Project-written bootstrap for the vendored Swagger UI (not upstream code).
// Same-origin only: the spec is /openapi.yaml and "Try it out" calls the
// relative server "/" of that spec. No credential is stored or embedded here;
// the user pastes a bearer value via Authorize (kept in page memory only).
window.onload = function () {
  const identities = new Map();
  window.ui = SwaggerUIBundle({
    url: "/openapi.yaml",
    dom_id: "#swagger-ui",
    deepLinking: false,
    tryItOutEnabled: true,
    persistAuthorization: false,
    validatorUrl: null,
    requestInterceptor: function (req) {
      if (!/^(POST|DELETE|PUT|PATCH)$/i.test(req.method)) return req;
      req.headers = req.headers || {};
      const header = name => Object.keys(req.headers).find(k => k.toLowerCase() === name);
      const principal = header("authorization"), acting = header("x-acting-user-id");
      const signature = JSON.stringify([req.method,req.url,req.body || "",principal ? req.headers[principal] : "",acting ? req.headers[acting] : ""]);
      const supplied = header("idempotency-key");
      if (supplied && req.headers[supplied]) { identities.set(signature,req.headers[supplied]); return req; }
      if (!identities.has(signature)) identities.set(signature,"swagger-" + crypto.randomUUID());
      req.headers["Idempotency-Key"] = identities.get(signature);
      return req;
    },
    docExpansion: "list",
    presets: [SwaggerUIBundle.presets.apis],
    layout: "BaseLayout"
  });
};
