
build the skeleton of a crypto exchange as follows
- config is a yml file (for now)
- operations are
[[
    use a lock per traded instrument
    this is LOB based
    - add limit order [contract code, price, volume, side]
    - execute market order (partial or full filling)
    - each operation can trigger extra processing (via function pointers)
    - when adding orders, trades can happen, and this will trigger a specific order
    - we can have zero volume orders or similar trick to add triggers on market prices
    - get LOB snapshot 
]]
- interface is using gRPC + https, give also client example (go, rust, python)
- management of margin calls
[[
    each order gets validated against the margin call checks (to be exposed as a black box I will implement)
    important: for each traded instrument, we must be able to check easily best bid / best ask / and last executed trades
]]
- command interface via websockets/https + GUI (maybe a TUI adapter later)
- for all https socket / grpc / websocket make sure there is some token base auth 