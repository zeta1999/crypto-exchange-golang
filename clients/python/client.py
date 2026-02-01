import json
import urllib.request

API = "http://localhost:8080"
TOKEN = "dev-secret-token"

order = {
    "instrument": "BTC-USD",
    "price": 42050,
    "volume": 0.2,
    "side": "sell",
    "client_id": "py-demo"
}

req = urllib.request.Request(
    f"{API}/orders",
    data=json.dumps(order).encode(),
    headers={
        "Content-Type": "application/json",
        "Authorization": f"Bearer {TOKEN}",
    },
)
with urllib.request.urlopen(req) as resp:
    body = json.loads(resp.read())
    print(json.dumps(body, indent=2))
