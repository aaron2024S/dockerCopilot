// 缓存版本号：升级这个值会让旧缓存被清理，从而让新版页面立即生效。
// v3：不再缓存 /api/ 响应，顺手清掉历史版本里被误缓存的接口数据。
const CACHE_NAME = 'docker-copilot-v3';

// 仅预缓存离线兜底必需的静态资源。
// 注意：不要预缓存 / 和 /index.html，否则页面骨架会被永久锁在安装时的那一版。
const urlsToCache = [
  '/logo.png',
  '/manifest.json'
];

// 判断是不是后端接口请求。前缀匹配要带上斜杠，避免把 /apixxx 这类路径也当成接口。
function isApiRequest(pathname) {
  return pathname === '/api' || pathname.startsWith('/api/');
}

// 安装事件
self.addEventListener('install', event => {
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then(cache => {
        return cache.addAll(urlsToCache);
      })
      .catch(err => {
        console.log('Cache open failed:', err);
      })
  );
  self.skipWaiting();
});

// 激活事件：清理所有旧版本缓存，并立即接管页面
self.addEventListener('activate', event => {
  event.waitUntil(
    caches.keys().then(cacheNames => {
      return Promise.all(
        cacheNames.map(cacheName => {
          if (cacheName !== CACHE_NAME) {
            return caches.delete(cacheName);
          }
        })
      );
    }).then(() => self.clients.claim())
  );
});

// 获取事件：网络优先，失败时回落缓存。
// 这样部署新版本后刷新即可看到新版，同时保留离线可用的能力。
//
// 例外：接口请求（/api/）完全交给浏览器，Service Worker 不介入。两个原因：
//   1) 缓存接口响应 = 拿到陈旧数据。检测状态、容器列表都是要实时的，
//      一旦命中缓存，前端会以为"还是那个状态"，轮询就失去意义；
//   2) 离线时的回落更危险 —— caches.match('/index.html') 会把一份 HTML
//      交给期望 JSON 的调用方，前端拿到的东西根本没法解析。
self.addEventListener('fetch', event => {
  // 只处理 GET 请求
  if (event.request.method !== 'GET') {
    return;
  }

  const requestUrl = new URL(event.request.url);
  // 只接管同源请求，其余交给浏览器
  if (requestUrl.origin !== self.location.origin) {
    return;
  }

  // 接口请求直接放行：不 respondWith，等于完全不走 SW 的缓存逻辑
  if (isApiRequest(requestUrl.pathname)) {
    return;
  }

  event.respondWith(
    fetch(event.request)
      .then(response => {
        // 仅缓存正常的同源响应
        if (response && response.status === 200 && response.type === 'basic') {
          const responseToCache = response.clone();
          caches.open(CACHE_NAME)
            .then(cache => {
              cache.put(event.request, responseToCache);
            });
        }
        return response;
      })
      .catch(() => {
        // 网络不可用时走缓存，仍不可用则回落到页面骨架
        return caches.match(event.request).then(cached => {
          if (cached) {
            return cached;
          }
          return caches.match('/index.html');
        });
      })
  );
});
