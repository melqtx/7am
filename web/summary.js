const KEY_UPDATES_ENABLED = "updatesEnabled"

const canReceiveUpdates = "Notification" in window && "serviceWorker" in navigator
const getSummaryButton = document.getElementById("get-summary-btn")
const loc = getSummaryButton.dataset.loc
getSummaryButton.style.display = "none"

console.log("can receive updates?", canReceiveUpdates)

if (canReceiveUpdates) {
    window.addEventListener("load", () => {
        navigator.serviceWorker.register("/sw.js")
    })

    navigator.serviceWorker.ready.then((reg) => {
        if (Notification.permission === "granted" && localStorage.getItem(KEY_UPDATES_ENABLED) === "true") {
            getSummaryButton.innerText = "Stop updates"
            reg.active.postMessage(loc)
        } else {
            getSummaryButton.innerText = "Get daily updates at 7am"
        }

        getSummaryButton.addEventListener("click", async () => {
            const currentlyEnabled = localStorage.getItem(KEY_UPDATES_ENABLED) === "true"
            const worker = await navigator.serviceWorker.ready
            if (currentlyEnabled) {
                localStorage.removeItem(KEY_UPDATES_ENABLED)
                worker.active.postMessage("cancel")
            } else if (await Notification.requestPermission()) {
                localStorage.setItem(KEY_UPDATES_ENABLED, "true")
                worker.active.postMessage(loc)
            } else {
                console.log("notification denied")
            }
        })

        getSummaryButton.style.display = "block"
    })
}
