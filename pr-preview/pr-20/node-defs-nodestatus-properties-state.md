# Untitled string in Node Schema

```txt
node.json#/$defs/NodeStatus/properties/state
```

Top-level health summary computed by the Controller Manager.

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [node.json\*](../../schemas/node.json "open original schema") |

## state Type

`string`

## state Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value        | Explanation |
| :----------- | :---------- |
| `"Ready"`    |             |
| `"NotReady"` |             |
| `"Draining"` |             |
