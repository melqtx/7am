const KEY_SUBSCRIPTION = "subscription"

const canReceiveUpdates = "Notification" in window && "serviceWorker" in navigator
const getSummaryButton = document.getElementById("get-summary-btn")
const loc = getSummaryButton.dataset.loc
getSummaryButton.style.display = "none"

async function main() {
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

    const existingSubscriptionJson = localStorage.getItem(KEY_SUBSCRIPTION)
    const existingSubscription = existingSubscriptionJson ? JSON.parse(existingSubscriptionJson) : null
    const currentlyEnabled = existingSubscription?.locations?.includes(loc) ?? false

    if (currentlyEnabled) {
        await reg.pushManager.getSubscription().then((sub) => sub?.unsubscribe())
        localStorage.removeItem(KEY_SUBSCRIPTION)
        getSummaryButton.innerText = "Get daily updates at 7am"
    } else {
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
            }).catch((error) => {
                console.error(error)
            })

            let newSubscription
            if (existingSubscription) {
                newSubscription = await fetch(`/registrations/${existingSubscription.id}`, {
                    method: "PATCH",
                    headers: {
                        "Content-Type": "application/json"
                    },
                    body: JSON.stringify({
                        subscription: pushSub,
                        locations: [...existingSubscription.locations, loc]
                    })
                }).then((res) => {
                    if (res.status === 200) {
                        return res.json()
                    }
                    throw new Error(`${res.status}`)
                })
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
                }).then((res) => {
                    if (res.status === 200) {
                        return res.json()
                    }
                    throw new Error(`${res.status}`)
                })
            }

            localStorage.setItem(KEY_SUBSCRIPTION, JSON.stringify(newSubscription))

            getSummaryButton.innerText = "Stop updates"
        } catch (error) {
            console.log(error)
        }
    }

}

if (canReceiveUpdates) {
    main()
}
