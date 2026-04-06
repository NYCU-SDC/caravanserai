# Untitled object in Project Schema

```txt
project.json#/$defs/ProjectStatus
```

ProjectStatus is written by the Controller Manager and Agent.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [project.json\*](../../schemas/project.json "open original schema") |

## ProjectStatus Type

`object` ([Details](project-defs-projectstatus.md))

# ProjectStatus Properties

| Property                  | Type     | Required | Nullable       | Defined by                                                                                                               |
| :------------------------ | :------- | :------- | :------------- | :----------------------------------------------------------------------------------------------------------------------- |
| [phase](#phase)           | `string` | Optional | cannot be null | [Project](project-defs-projectphase.md "project.json#/$defs/ProjectStatus/properties/phase")                             |
| [nodeRef](#noderef)       | `string` | Optional | cannot be null | [Project](project-defs-projectstatus-properties-noderef.md "project.json#/$defs/ProjectStatus/properties/nodeRef")       |
| [conditions](#conditions) | `array`  | Optional | cannot be null | [Project](project-defs-projectstatus-properties-conditions.md "project.json#/$defs/ProjectStatus/properties/conditions") |

## phase

Lifecycle state of a Project as maintained by the Controller Manager.

`phase`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-projectphase.md "project.json#/$defs/ProjectStatus/properties/phase")

### phase Type

`string`

### phase Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value           | Explanation |
| :-------------- | :---------- |
| `"Pending"`     |             |
| `"Scheduled"`   |             |
| `"Running"`     |             |
| `"Failed"`      |             |
| `"Terminating"` |             |
| `"Terminated"`  |             |

## nodeRef

NodeRef is the name of the Node the Scheduler chose. Empty while Pending.

`nodeRef`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-projectstatus-properties-noderef.md "project.json#/$defs/ProjectStatus/properties/nodeRef")

### nodeRef Type

`string`

## conditions

Conditions is a list of granular observable states.

`conditions`

* is optional

* Type: `object[]` ([Details](project-defs-condition.md))

* cannot be null

* defined in: [Project](project-defs-projectstatus-properties-conditions.md "project.json#/$defs/ProjectStatus/properties/conditions")

### conditions Type

`object[]` ([Details](project-defs-condition.md))
