// Project-written bootstrap for the vendored Swagger UI (not upstream code).
// Same-origin only: the spec is /openapi.yaml and "Try it out" calls the
// relative server "/" of that spec. No credential is stored or embedded here;
// the user pastes a bearer value via Authorize (kept in page memory only).
window.onload = function () {
  window.ui = SwaggerUIBundle({
    url: "/openapi.yaml",
    dom_id: "#swagger-ui",
    deepLinking: false,
    tryItOutEnabled: true,
    persistAuthorization: false,
    validatorUrl: null,
    docExpansion: "list",
    presets: [SwaggerUIBundle.presets.apis],
    layout: "BaseLayout"
  });
};
