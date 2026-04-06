# Untitled boolean in Node Schema

```txt
node.json#/$defs/NodeSpec/properties/unschedulable
```

Unschedulable, when true, prevents new Projects from being scheduled
onto this Node. The TaintsController will automatically add a
NoSchedule Taint when this field is true.

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [node.json\*](../../schemas/node.json "open original schema") |

## unschedulable Type

`boolean`
