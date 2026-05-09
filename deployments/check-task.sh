#!/bin/bash
TASK_ARN=$(aws ecs list-tasks --cluster rategate --region us-east-1 --query "taskArns[0]" --output text)
aws ecs describe-tasks --cluster rategate --tasks $TASK_ARN --region us-east-1 --query "tasks[0].{status:lastStatus,reason:stoppedReason,container:containers[0].reason}" --output json
