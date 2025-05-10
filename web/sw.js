self.addEventListener('install', function (event) {
    event.waitUntil(self.skipWaiting());
});

self.addEventListener('activate', function (event) {
    event.waitUntil(self.clients.claim());
});

self.addEventListener("push", (event) => {
    if (event.data) {
        const { summary, location } = event.data.json()
        event.waitUntil(
            self.registration.showNotification("7am weather summary", {
                data: location,
                body: summary,
            })
        )
    }
})

self.addEventListener("notificationclick", (event) => {
    event.notification.close()
    event.waitUntil(
        self.clients.matchAll({ type: "window" })
            .then((clientList) => {
                const loc = event.notification.data
                for (const client of clientList) {
                    if (client.url === `/${loc}` && "focus" in client) return client.focus();
                }
                if (self.clients.openWindow) return self.clients.openWindow(`/${loc}`);
            })
    )
})
