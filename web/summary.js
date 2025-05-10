const KEY_SUBSCRIPTION = "subscription"

const canReceiveUpdates = "serviceWorker" in navigator
const getSummaryButton = document.getElementById("get-summary-btn")
const loc = getSummaryButton.dataset.loc

async function main() {
    getSummaryButton.style.display = "none"

    window.addEventListener("load", () => {
        navigator.serviceWorker.register("/sw.js")
    })

    const reg = await navigator.serviceWorker.ready

    const existingSubscriptionJson = localStorage.getItem(KEY_SUBSCRIPTION)
    const existingSubscription = existingSubscriptionJson ? JSON.parse(existingSubscriptionJson) : null

    if (existingSubscription?.locations?.includes(loc) ?? false) {
        getSummaryButton.innerText = "Stop updates"
        reg.active.postMessage(loc)
    } else {
        getSummaryButton.innerText = "Get daily updates at 7am"
    }

    getSummaryButton.addEventListener("click", onButtonClick)
    getSummaryButton.style.display = "block"
}

async function onButtonClick() {
    const reg = await navigator.serviceWorker.ready

    const pushSub = await reg.pushManager.getSubscription()
    const existingSubscriptionJson = localStorage.getItem(KEY_SUBSCRIPTION)
    const registeredSubscription = existingSubscriptionJson ? JSON.parse(existingSubscriptionJson) : null
    const currentlyEnabled = (registeredSubscription?.locations?.includes(loc) ?? false) && pushSub !== null

    if (currentlyEnabled) {
        registeredSubscription.locations.splice(
            registeredSubscription.locations.indexOf(loc),
            1
        )
        if (registeredSubscription.locations.length === 0) {
            await reg.pushManager.getSubscription().then((sub) => sub?.unsubscribe())
            await fetch(`/registrations/${registeredSubscription.id}`, { method: "DELETE" })
            localStorage.removeItem(KEY_SUBSCRIPTION)
        } else {
            const newReg = await fetch(`/registrations/${registeredSubscription.id}`, {
                method: "PATCH",
                headers: {
                    "Content-Type": "application/json"
                },
                body: JSON.stringify({
                    subscription: pushSub,
                    locations: registeredSubscription.locations,
                })
            }).then(jsonOrThrow)
            localStorage.setItem(KEY_SUBSCRIPTION, newReg)
        }
        getSummaryButton.innerText = "Get daily updates at 7am"
    } else {
        getSummaryButton.innerText = "Subscribing"
        getSummaryButton.disabled = true

        const worker = await navigator.serviceWorker.ready

        try {
            const publicKey = await fetch("/vapid").then((res) => {
                if (res.status === 200) {
                    return res.text()
                }
                throw new Error(`${res.status}`)
            })
            const pushSub = await worker.pushManager.subscribe({
                userVisibleOnly: true,
                applicationServerKey: publicKey
            })

            let newSubscription
            if (registeredSubscription) {
                newSubscription = await fetch(`/registrations/${registeredSubscription.id}`, {
                    method: "PATCH",
                    headers: {
                        "Content-Type": "application/json"
                    },
                    body: JSON.stringify({
                        subscription: pushSub,
                        locations: [loc],
                    })
                }).then(jsonOrThrow)
            } else {
                newSubscription = await fetch("/registrations", {
                    method: "POST",
                    headers: {
                        "Content-Type": "application/json"
                    },
                    body: JSON.stringify({
                        subscription: pushSub,
                        locations: [loc]
                    })
                }).then(jsonOrThrow)
            }

            localStorage.setItem(KEY_SUBSCRIPTION, JSON.stringify(newSubscription))

            getSummaryButton.innerText = "Stop updates"
        } catch (error) {
            console.error(error)
            alert(`Error when trying to subscribe to updates: ${error}`)
            getSummaryButton.innerText = "Get daily updates at 7am"
        } finally {
            getSummaryButton.disabled = false
        }
    }
}

function jsonOrThrow(res) {
    if (res.status === 200) {
        return res.json()
    }
    throw new Error(`server returned status ${res.status}`)
}

if (canReceiveUpdates) {
    main()
}
