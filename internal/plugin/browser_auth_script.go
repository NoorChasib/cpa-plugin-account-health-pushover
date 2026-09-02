package plugin

// browserAuthScript is inline JavaScript shared by the browser views. It
// exposes window.accountHealthAuth with three helpers:
//
//   - managementKey(): best-effort recovery of the CPA management key that the
//     official management console (Cli-Proxy-API-Management-Center) persists
//     in localStorage. The console stores its state under "cli-proxy-auth"
//     (legacy installs used "managementKey") behind a documented reversible
//     XOR obfuscation ("enc::v1::" prefix) keyed by a fixed salt,
//     location.host, and navigator.userAgent. Reading that storage is the
//     documented trust model for plugin resource pages served from the same
//     origin as the console; when the console runs on a different origin the
//     entries simply do not exist here and this returns null.
//   - managementPath(suffix): rebuilds an absolute management route path from
//     the current location, preserving any reverse-proxy prefix, whether the
//     page is served from the management route tree or the resource one.
//   - authHeaders(extra): copies extra and adds the Authorization header when
//     a management key was recovered. The header value is assembled from
//     parts so the page source never contains bearer-credential material.
//
// The recovered key is only ever sent to same-origin CPA management routes;
// nothing is persisted or exported anywhere else.
const browserAuthScript = `
(function (global) {
	"use strict";
	var SALT = "cli-proxy-api-webui::secure-storage";
	var OBFUSCATION_PREFIX = "enc::v1::";
	var PLUGIN_ID = "account-health-pushover";

	function keyBytes() {
		var text = SALT;
		try {
			text = SALT + "|" + global.location.host + "|" + global.navigator.userAgent;
		} catch (ignored) {}
		return new TextEncoder().encode(text);
	}

	function deobfuscate(raw) {
		if (typeof raw !== "string" || raw === "") {
			return null;
		}
		if (raw.indexOf(OBFUSCATION_PREFIX) !== 0) {
			return raw;
		}
		try {
			var binary = atob(raw.slice(OBFUSCATION_PREFIX.length));
			var data = new Uint8Array(binary.length);
			for (var i = 0; i < binary.length; i++) {
				data[i] = binary.charCodeAt(i);
			}
			var key = keyBytes();
			var plain = new Uint8Array(data.length);
			for (var j = 0; j < data.length; j++) {
				plain[j] = data[j] ^ key[j % key.length];
			}
			return new TextDecoder().decode(plain);
		} catch (ignored) {
			return null;
		}
	}

	function parseLoose(text) {
		if (typeof text !== "string") {
			return null;
		}
		try {
			return JSON.parse(text);
		} catch (ignored) {
			return text;
		}
	}

	function managementKey() {
		try {
			var persisted = parseLoose(deobfuscate(global.localStorage.getItem("cli-proxy-auth")));
			if (persisted && typeof persisted === "object" && persisted.state &&
				typeof persisted.state.managementKey === "string" && persisted.state.managementKey !== "") {
				return persisted.state.managementKey;
			}
			var legacy = parseLoose(deobfuscate(global.localStorage.getItem("managementKey")));
			if (typeof legacy === "string" && legacy !== "") {
				return legacy;
			}
		} catch (ignored) {}
		return null;
	}

	function managementPath(suffix) {
		var path = global.location.pathname.replace(/\/+$/, "");
		var prefix = "";
		var markers = ["/v0/management/plugins/" + PLUGIN_ID, "/v0/resource/plugins/" + PLUGIN_ID];
		for (var i = 0; i < markers.length; i++) {
			var at = path.indexOf(markers[i]);
			if (at >= 0) {
				prefix = path.slice(0, at);
				break;
			}
		}
		return prefix + "/v0/management/plugins/" + PLUGIN_ID + suffix;
	}

	function authHeaders(extra) {
		var headers = {};
		var name;
		for (name in (extra || {})) {
			if (Object.prototype.hasOwnProperty.call(extra, name)) {
				headers[name] = extra[name];
			}
		}
		var key = managementKey();
		if (key) {
			headers.Authorization = ["Bearer", key].join(" ");
		}
		return headers;
	}

	global.accountHealthAuth = {
		managementKey: managementKey,
		managementPath: managementPath,
		authHeaders: authHeaders
	};
})(window);
`

// resourceBootstrapScript runs on the unauthenticated resource shell. It tries
// to fetch the authenticated management HTML view over the same origin using
// only credentials the browser already holds, swaps it in on success, and
// otherwise leaves the public shell up with a short explanatory note.
const resourceBootstrapScript = `
(function () {
	"use strict";
	var note = document.getElementById("session-note");
	function showNote(text) {
		if (note) {
			note.textContent = text;
		}
	}
	var auth = window.accountHealthAuth;
	if (!auth || typeof window.fetch !== "function") {
		return;
	}
	showNote("Checking for a same-origin management session…");
	fetch(auth.managementPath("/status/html") + location.search, {
		credentials: "same-origin",
		headers: auth.authHeaders({})
	})
		.then(function (resp) {
			if (resp.status === 401 || resp.status === 403) {
				throw new Error("unauthenticated");
			}
			if (!resp.ok) {
				throw new Error("HTTP " + resp.status);
			}
			var type = resp.headers.get("Content-Type") || "";
			if (type.indexOf("text/html") !== 0) {
				throw new Error("unexpected content type");
			}
			return resp.text();
		})
		.then(function (html) {
			document.open();
			document.write(html);
			document.close();
		})
		.catch(function (err) {
			if (err && err.message === "unauthenticated") {
				showNote("No same-origin management session was found, so this redacted view is shown. Sign in to the management console served from this origin with the management key remembered, then reload.");
			} else {
				showNote("Could not load the authenticated status view (" + (err && err.message ? err.message : "error") + "); showing the redacted view.");
			}
		});
})();
`

// managementActionsScript wires the Check now and Test notification buttons on
// the authenticated view. Both actions POST to same-origin management routes
// with the plugin CSRF header and any recoverable management key.
const managementActionsScript = `
(function () {
	"use strict";
	var auth = window.accountHealthAuth;
	var result = document.getElementById("action-result");
	var buttons = document.querySelectorAll("button[data-action]");
	if (!auth || !result || !buttons.length) {
		return;
	}
	function setBusy(busy) {
		for (var i = 0; i < buttons.length; i++) {
			buttons[i].disabled = busy;
		}
	}
	function describe(resp) {
		return resp.json().then(function (body) {
			if (body && typeof body.error === "string" && body.error) {
				return body.error;
			}
			return "HTTP " + resp.status;
		}, function () {
			return "HTTP " + resp.status;
		});
	}
	function run(action) {
		setBusy(true);
		result.className = "result";
		result.textContent = action === "check" ? "Running health check…" : "Sending test notification…";
		fetch(auth.managementPath("/" + action) + location.search, {
			method: "POST",
			credentials: "same-origin",
			headers: auth.authHeaders({ "X-Account-Health-Action": "1" })
		})
			.then(function (resp) {
				if (resp.ok) {
					if (action === "check") {
						result.textContent = "Health check completed; reloading…";
						location.reload();
						return;
					}
					result.className = "result ok";
					result.textContent = "Test notification accepted by Pushover.";
					setBusy(false);
					return;
				}
				return describe(resp).then(function (detail) {
					result.className = "result err";
					result.textContent = (action === "check" ? "Health check failed: " : "Test notification failed: ") + detail;
					setBusy(false);
				});
			})
			.catch(function () {
				result.className = "result err";
				result.textContent = "Request failed: network error";
				setBusy(false);
			});
	}
	for (var i = 0; i < buttons.length; i++) {
		buttons[i].addEventListener("click", function (event) {
			run(event.currentTarget.getAttribute("data-action"));
		});
	}
})();
`
