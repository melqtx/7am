self.addEventListener('install', function (event) {
    event.waitUntil(self.skipWaiting());
});

self.addEventListener('activate', function (event) {
    event.waitUntil(self.clients.claim());
});

self.addEventListener("push", (event) => {
    if (event.data) {
        event.waitUntil(
            self.registration.showNotification("7am weather summary", {
                body: event.data.text()
            })
        )
    }
})
