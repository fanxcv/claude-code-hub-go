# Management REST API v1

`/api/v1/*` is the REST management API for CC Hub Go. Its HTTP surface is
mounted separately from `/v1/*`, the Claude/OpenAI-compatible proxy endpoints.

The legacy Server Action adapter (`/api/actions/*`) was removed together with the
Node backend; this Go backend serves no such surface, and every path under
`/api/actions/` answers 404.

## Documentation

- OpenAPI JSON: `/api/v1/openapi.json`
- Scalar UI: `/api/v1/scalar`
- Swagger UI: `/api/v1/docs`

Every response includes `X-API-Version: 1.0.0`.

## Authentication

The API accepts three credential transports:

- Browser session cookie: `auth-token=<session>`.
- Bearer token: `Authorization: Bearer <token>`.
- API key header: `X-Api-Key: <key>`.

Access tiers:

- `public`: no authentication required. Example: `GET /api/v1/public/status`.
- `read`: accepts a valid session, `ADMIN_TOKEN`, or any valid user API key.
- `admin`: accepts a valid session cookie, opaque session bearer token, and `ADMIN_TOKEN` by default. User API keys are rejected unless `ENABLE_API_KEY_ADMIN_ACCESS=true` and the key belongs to an admin user.

Cookie-authenticated mutations must first call `GET /api/v1/auth/csrf` and send the returned token in `X-CCH-CSRF`.
Set `CSRF_SECRET` in production, and use the same value on every replica.

## Error Format

Failures use RFC 9457-style `application/problem+json`:

```json
{
  "type": "urn:claude-code-hub:problem:auth.forbidden",
  "title": "Forbidden",
  "status": 403,
  "detail": "Admin access is required.",
  "instance": "/api/v1/providers",
  "errorCode": "auth.forbidden",
  "errorParams": {}
}
```

Frontend code should localize by `errorCode` and `errorParams`, not display `detail` directly.
