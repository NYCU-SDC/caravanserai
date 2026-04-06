# Untitled string in Node Schema

```txt
node.json#/$defs/NodeNetworkStatus/properties/mode
```

Connectivity mode reported by the Tailscale/Headscale overlay network.

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [node.json\*](../../schemas/node.json "open original schema") |

## mode Type

`string`

## mode Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value      | Explanation |
| :--------- | :---------- |
| `"Direct"` |             |
| `"DERP"`   |             |
