let ws
let shouldRetry = false

function start(location) {
    shouldRetry = true
    const port = self.location.port ? `:${self.location.port}` : ""
    const socket = new WebSocket(`ws://${self.location.hostname}${port}/ws?location=${location}`)
    socket.onopen = () => {
        ws = socket
    }
    socket.onclose = () => {
        reconnect()
    }
    socket.onerror = () => {
        reconnect()
    }
    socket.onmessage = (event) => {
        self.registration.showNotification("7am weather summary", {
            body: event.data
        })
    }
}

function cancel() {
    shouldRetry = false
    if (ws) {
        ws.close()
        ws = null
    }
}

async function reconnect() {
    while (shouldRetry) {
        await new Promise((resolve) => {
            setTimeout(resolve, 5 * 60 * 1000)
        })
        start(location)
    }
}

self.addEventListener("message", (event) => {
    if (event.data === "cancel") {
        cancel()
    } else {
        start(event.data)
    }
})