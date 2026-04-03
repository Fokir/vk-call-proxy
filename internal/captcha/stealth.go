package captcha

// stealthJS is injected before any page JavaScript runs.
// It patches browser APIs to make headless Chrome indistinguishable
// from a regular Chrome with a real display.
const stealthJS = `
// 1. navigator.webdriver = false
Object.defineProperty(navigator, 'webdriver', {
  get: () => false,
  configurable: true,
});

// 2. chrome object (present in real Chrome, absent in headless)
if (!window.chrome) {
  window.chrome = {};
}
window.chrome.runtime = window.chrome.runtime || {};
window.chrome.loadTimes = window.chrome.loadTimes || function() {
  return {
    requestTime: Date.now() / 1000,
    startLoadTime: Date.now() / 1000,
    commitLoadTime: Date.now() / 1000,
    finishDocumentLoadTime: Date.now() / 1000,
    finishLoadTime: Date.now() / 1000,
    firstPaintTime: Date.now() / 1000,
    firstPaintAfterLoadTime: 0,
    navigationType: "Other",
    wasFetchedViaSpdy: false,
    wasNpnNegotiated: false,
    npnNegotiatedProtocol: "unknown",
    wasAlternateProtocolAvailable: false,
    connectionInfo: "h2",
  };
};
window.chrome.csi = window.chrome.csi || function() {
  return {
    onloadT: Date.now(),
    startE: Date.now(),
    pageT: Date.now() - performance.timing.navigationStart,
    tran: 15,
  };
};

// 3. Plugins (empty in headless, populated in real Chrome)
Object.defineProperty(navigator, 'plugins', {
  get: () => {
    const plugins = [
      { name: 'Chrome PDF Plugin', filename: 'internal-pdf-viewer', description: 'Portable Document Format' },
      { name: 'Chrome PDF Viewer', filename: 'mhjfbmdgcfjbbpaeojofohoefgiehjai', description: '' },
      { name: 'Native Client', filename: 'internal-nacl-plugin', description: '' },
    ];
    plugins.length = 3;
    return plugins;
  },
  configurable: true,
});

// 4. Languages
Object.defineProperty(navigator, 'languages', {
  get: () => ['ru-RU', 'ru', 'en-US', 'en'],
  configurable: true,
});

// 5. Platform
Object.defineProperty(navigator, 'platform', {
  get: () => 'Win32',
  configurable: true,
});

// 6. Hardware concurrency (0 in some headless configs)
if (navigator.hardwareConcurrency === 0 || navigator.hardwareConcurrency === undefined) {
  Object.defineProperty(navigator, 'hardwareConcurrency', {
    get: () => 8,
    configurable: true,
  });
}

// 7. Device memory
if (!navigator.deviceMemory) {
  Object.defineProperty(navigator, 'deviceMemory', {
    get: () => 8,
    configurable: true,
  });
}

// 8. WebGL vendor/renderer (headless returns "Google Inc." / "Google SwiftShader")
const origGetParameter = WebGLRenderingContext.prototype.getParameter;
WebGLRenderingContext.prototype.getParameter = function(param) {
  // UNMASKED_VENDOR_WEBGL
  if (param === 37445) return 'Intel Inc.';
  // UNMASKED_RENDERER_WEBGL
  if (param === 37446) return 'Intel Iris OpenGL Engine';
  return origGetParameter.call(this, param);
};
// Same for WebGL2
if (typeof WebGL2RenderingContext !== 'undefined') {
  const origGetParameter2 = WebGL2RenderingContext.prototype.getParameter;
  WebGL2RenderingContext.prototype.getParameter = function(param) {
    if (param === 37445) return 'Intel Inc.';
    if (param === 37446) return 'Intel Iris OpenGL Engine';
    return origGetParameter2.call(this, param);
  };
}

// 9. Permissions — notifications query returns "denied" not "prompt"
const origQuery = window.Permissions && window.Permissions.prototype.query;
if (origQuery) {
  window.Permissions.prototype.query = function(parameters) {
    if (parameters.name === 'notifications') {
      return Promise.resolve({ state: Notification.permission || 'denied' });
    }
    return origQuery.call(this, parameters);
  };
}

// 10. Connection API (absent in headless)
if (!navigator.connection) {
  Object.defineProperty(navigator, 'connection', {
    get: () => ({
      effectiveType: '4g',
      rtt: 100,
      downlink: 10,
      saveData: false,
    }),
    configurable: true,
  });
}

// 11. Remove automation-related properties
delete navigator.__proto__.webdriver;
for (const key of Object.keys(window)) {
  if (key.match(/^cdc_/) || key.match(/^\$cdc_/) || key.match(/^_phantom/) || key.match(/^_selenium/)) {
    delete window[key];
  }
}

// 12. Notification.permission
if (typeof Notification !== 'undefined' && Notification.permission === 'default') {
  Object.defineProperty(Notification, 'permission', {
    get: () => 'denied',
    configurable: true,
  });
}

// 13. Screen dimensions consistency
if (screen.width === 0 || screen.height === 0) {
  Object.defineProperty(screen, 'width', { get: () => 1920 });
  Object.defineProperty(screen, 'height', { get: () => 1080 });
  Object.defineProperty(screen, 'availWidth', { get: () => 1920 });
  Object.defineProperty(screen, 'availHeight', { get: () => 1040 });
  Object.defineProperty(screen, 'colorDepth', { get: () => 24 });
  Object.defineProperty(screen, 'pixelDepth', { get: () => 24 });
}
`
