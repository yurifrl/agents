/** Error thrown by surfaces; `code` lets the server pick a fallback policy. */
export class SurfaceError extends Error {
  readonly code: string;

  constructor(code: string, message?: string, cause?: unknown) {
    super(message ?? code, cause !== undefined ? { cause } : undefined);
    this.name = "SurfaceError";
    this.code = code;
  }
}
