# Untitled string in Project Schema

```txt
project.json#/$defs/ProjectStatus/properties/phase
```

Lifecycle state of a Project as maintained by the Controller Manager.

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [project.json\*](../../schemas/project.json "open original schema") |

## phase Type

`string`

## phase Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value           | Explanation |
| :-------------- | :---------- |
| `"Pending"`     |             |
| `"Scheduled"`   |             |
| `"Running"`     |             |
| `"Failed"`      |             |
| `"Terminating"` |             |
| `"Terminated"`  |             |
