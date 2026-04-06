# Untitled string in Project Schema

```txt
project.json#/$defs/IngressAccess/properties/scope
```

Controls whether a route is exposed to the public internet or only to the Headscale overlay network.

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [project.json\*](../../schemas/project.json "open original schema") |

## scope Type

`string`

## scope Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value        | Explanation |
| :----------- | :---------- |
| `"Internal"` |             |
