/**
 * @license
 * Copyright 2026 Google LLC
 * SPDX-License-Identifier: Apache-2.0
 */
var __addDisposableResource = (this && this.__addDisposableResource) || function (env, value, async) {
    if (value !== null && value !== void 0) {
        if (typeof value !== "object" && typeof value !== "function") throw new TypeError("Object expected.");
        var dispose, inner;
        if (async) {
            if (!Symbol.asyncDispose) throw new TypeError("Symbol.asyncDispose is not defined.");
            dispose = value[Symbol.asyncDispose];
        }
        if (dispose === void 0) {
            if (!Symbol.dispose) throw new TypeError("Symbol.dispose is not defined.");
            dispose = value[Symbol.dispose];
            if (async) inner = dispose;
        }
        if (typeof dispose !== "function") throw new TypeError("Object not disposable.");
        if (inner) dispose = function() { try { inner.call(this); } catch (e) { return Promise.reject(e); } };
        env.stack.push({ value: value, dispose: dispose, async: async });
    }
    else if (async) {
        env.stack.push({ async: true });
    }
    return value;
};
var __disposeResources = (this && this.__disposeResources) || (function (SuppressedError) {
    return function (env) {
        function fail(e) {
            env.error = env.hasError ? new SuppressedError(e, env.error, "An error was suppressed during disposal.") : e;
            env.hasError = true;
        }
        var r, s = 0;
        function next() {
            while (r = env.stack.pop()) {
                try {
                    if (!r.async && s === 1) return s = 0, env.stack.push(r), Promise.resolve().then(next);
                    if (r.dispose) {
                        var result = r.dispose.call(r.value);
                        if (r.async) return s |= 2, Promise.resolve(result).then(next, function(e) { fail(e); return next(); });
                    }
                    else s |= 1;
                }
                catch (e) {
                    fail(e);
                }
            }
            if (s === 1) return env.hasError ? Promise.reject(env.error) : Promise.resolve();
            if (env.hasError) throw env.error;
        }
        return next();
    };
})(typeof SuppressedError === "function" ? SuppressedError : function (error, suppressed, message) {
    var e = new Error(message);
    return e.name = "SuppressedError", e.error = error, e.suppressed = suppressed, e;
});
import { McpResponse } from './McpResponse.js';
import { SlimMcpResponse } from './SlimMcpResponse.js';
import { ClearcutLogger } from './telemetry/ClearcutLogger.js';
import { zod } from './third_party/index.js';
import { labels } from './tools/categories.js';
import { categoryToFlagName } from './config/category-options.js';
import { logger } from './utils/logger.js';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { isLocalhost } from './utils/url.js';
function buildDisabledMessage(toolName, flag, categoryLabel) {
    const reason = categoryLabel
        ? `is in category ${categoryLabel} which`
        : `requires ${flag.startsWith('--experimental') ? 'experimental feature' : 'flag'} ${flag} and`;
    return `Tool ${toolName} ${reason} is currently disabled. Enable it by running chrome-devtools start ${flag}=true. For more information check the README.`;
}
function getToolStatusInfo(tool, serverArgs) {
    const category = tool.annotations.category;
    if (category) {
        const flag = categoryToFlagName(category);
        if (!serverArgs[flag]) {
            return {
                disabled: true,
                reason: buildDisabledMessage(tool.name, `--${flag}`, labels[category]),
            };
        }
    }
    for (const condition of tool.annotations.conditions || []) {
        if (!serverArgs[condition]) {
            return {
                disabled: true,
                reason: buildDisabledMessage(tool.name, `--${condition}`),
            };
        }
    }
    return { disabled: false };
}
function isPageScopedTool(tool) {
    return 'pageScoped' in tool && tool.pageScoped === true;
}
function formatArgumentNames(names) {
    return names.map(name => `"${name}"`).join(', ');
}
function buildUnknownArgumentsMessage(toolName, unknownArgumentNames, expectedArgumentNames) {
    const unknownLabel = unknownArgumentNames.length === 1 ? 'argument' : 'arguments';
    const expectedArguments = expectedArgumentNames.length
        ? `Expected arguments: ${formatArgumentNames(expectedArgumentNames)}.`
        : 'This tool does not accept any arguments.';
    const correction = unknownArgumentNames.length === 1 ? 'Remove it' : 'Remove them';
    return `Unknown ${unknownLabel} for tool "${toolName}": ${formatArgumentNames(unknownArgumentNames)}. ${expectedArguments} ${correction} and retry.`;
}
async function validateAndResolvePathOrUrl(filePathOrUrl, context) {
    try {
        const url = new URL(filePathOrUrl);
        if (url.protocol === 'file:') {
            return pathToFileURL(await context.validatePath(fileURLToPath(url))).href;
        }
        else if (['http:', 'https:', 'ws:', 'wss:'].includes(url.protocol)) {
            return filePathOrUrl;
        }
    }
    catch {
        // Suppress parsing errors for regular file paths.
    }
    return await context.validatePath(filePathOrUrl);
}
function isLocalBrowser(context) {
    if (context.browser.process()) {
        return true;
    }
    const wsEndpoint = context.browser.wsEndpoint();
    if (wsEndpoint && isLocalhost(wsEndpoint)) {
        return true;
    }
    return false;
}
function shouldValidateFile(option, isLocal) {
    if (option === true) {
        return true;
    }
    if (typeof option === 'object' && option !== null) {
        if (isLocal) {
            return Boolean(option.local);
        }
        return Boolean(option.remote);
    }
    return false;
}
async function validateToolFiles(tool, params, context) {
    const isLocal = isLocalBrowser(context);
    for (const [key, option] of Object.entries(tool.verifyFilesSchema)) {
        if (shouldValidateFile(option, isLocal)) {
            const val = params[key];
            if (typeof val === 'string') {
                params[key] = await validateAndResolvePathOrUrl(val, context);
            }
            else if (Array.isArray(val)) {
                const updated = [];
                for (const item of val) {
                    if (typeof item === 'string') {
                        updated.push(await validateAndResolvePathOrUrl(item, context));
                    }
                    else {
                        throw new Error('Unexpected non-string value as a file path or URL');
                    }
                }
                params[key] = updated;
            }
        }
    }
}
export class ToolHandler {
    tool;
    serverArgs;
    getContext;
    toolMutex;
    inputSchema;
    registeredInputSchema;
    disabled;
    disabledReason;
    constructor(tool, serverArgs, getContext, toolMutex) {
        this.tool = tool;
        this.serverArgs = serverArgs;
        this.getContext = getContext;
        this.toolMutex = toolMutex;
        const { disabled, reason } = getToolStatusInfo(tool, serverArgs);
        this.disabledReason = reason;
        this.disabled = disabled && !serverArgs.viaCli;
        this.inputSchema = tool.schema;
        this.registeredInputSchema = zod.object(this.inputSchema).loose();
    }
    unknownArgumentNames(params) {
        return Object.keys(params).filter(key => !Object.hasOwn(this.inputSchema, key));
    }
    handle = async (params) => {
        const env_1 = { stack: [], error: void 0, hasError: false };
        try {
            const _guard = __addDisposableResource(env_1, await this.toolMutex.acquire(), false);
            if (this.disabledReason) {
                return {
                    content: [
                        {
                            type: 'text',
                            text: this.disabledReason,
                        },
                    ],
                    isError: true,
                };
            }
            const unknownArgumentNames = this.unknownArgumentNames(params);
            if (unknownArgumentNames.length) {
                return {
                    content: [
                        {
                            type: 'text',
                            text: buildUnknownArgumentsMessage(this.tool.name, unknownArgumentNames, Object.keys(this.inputSchema)),
                        },
                    ],
                    isError: true,
                };
            }
            const startTime = Date.now();
            let success = false;
            let devToolsData;
            let pageUrl;
            try {
                logger?.(`${this.tool.name} request: ${JSON.stringify(params, null, '  ')}`);
                const context = await this.getContext();
                logger?.(`${this.tool.name} context: resolved`);
                const response = this.serverArgs.slim
                    ? new SlimMcpResponse(this.serverArgs)
                    : new McpResponse(this.serverArgs);
                response.setRedactNetworkHeaders(this.serverArgs.redactNetworkHeaders);
                if (context.consumeReconnectNotice()) {
                    response.setReconnectNotice();
                }
                let page;
                try {
                    await validateToolFiles(this.tool, params, context);
                    if (isPageScopedTool(this.tool)) {
                        const pageId = typeof params.pageId === 'number' ? params.pageId : undefined;
                        page =
                            this.serverArgs.pageIdRouting &&
                                pageId !== undefined &&
                                !this.serverArgs.slim
                                ? context.getPageById(pageId)
                                : context.getSelectedMcpPage();
                        response.setPage(page);
                        if (this.tool.blockedByDialog) {
                            page.throwIfDialogOpen();
                        }
                        await this.tool.handler({
                            params,
                            page,
                        }, response, context);
                    }
                    else {
                        await this.tool.handler({
                            params,
                        }, response, context);
                    }
                }
                catch (err) {
                    response.setError(err);
                }
                devToolsData = await context.getDevToolsData(page);
                pageUrl = context.getSelectedMcpPageUrl(page);
                // Resolve data format: --experimentalDataFormat takes precedence, fall back to legacy --experimentalToonFormat
                let dataFormat = 'default';
                if (this.serverArgs.experimentalDataFormat) {
                    dataFormat = this.serverArgs.experimentalDataFormat;
                }
                else if (this.serverArgs.experimentalToonFormat) {
                    dataFormat = 'toon';
                }
                const { content, structuredContent } = await response.handle(context, dataFormat);
                const result = {
                    content,
                };
                if (response.error) {
                    result.isError = true;
                }
                success = true;
                if (this.serverArgs.experimentalStructuredContent) {
                    result.structuredContent = structuredContent;
                }
                return result;
            }
            catch (err) {
                logger?.(`${this.tool.name} error:`, err, err?.stack);
                let errorText = err && 'message' in err ? err.message : String(err);
                if ('cause' in err && err.cause) {
                    errorText += `\nCause: ${err.cause.message}`;
                }
                return {
                    content: [
                        {
                            type: 'text',
                            text: errorText,
                        },
                    ],
                    isError: true,
                };
            }
            finally {
                void ClearcutLogger.get()?.logToolInvocation({
                    toolName: this.tool.name,
                    params,
                    schema: this.inputSchema,
                    success,
                    latencyMs: Date.now() - startTime,
                    devToolsData,
                    pageUrl,
                });
            }
        }
        catch (e_1) {
            env_1.error = e_1;
            env_1.hasError = true;
        }
        finally {
            __disposeResources(env_1);
        }
    };
}
//# sourceMappingURL=ToolHandler.js.map